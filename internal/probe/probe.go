// Package probe implements bounded semantic health checks for relay protocols.
// A target is CONFIRMED only after a 2xx response passes protocol-shape
// validation. Response bodies and plaintext credentials never leave this
// package.
package probe

import (
	"context"
	"time"
)

// Protocol identifies the endpoint being probed.
type Protocol string

const (
	ProtocolModels    Protocol = "models"    // /v1/models
	ProtocolResponses Protocol = "responses" // /v1/responses
	ProtocolMessages  Protocol = "messages"  // /v1/messages
)

// Status is the semantic outcome of a probe.
type Status string

const (
	StatusConfirmed   Status = "CONFIRMED"
	StatusUnconfirmed Status = "UNCONFIRMED"
)

// ErrClass determines retry behavior and the actionable failure category.
type ErrClass int

const (
	ClassNone       ErrClass = iota
	ClassCredential          // 401/403; never retry
	ClassProtocol            // unsupported endpoint or invalid protocol shape
	ClassBackoff             // 429/502/503 and retryable transport failures
	ClassTransient           // non-retryable transport/cancellation failure
)

// Result describes a completed semantic probe. OK and Unconfirmed are kept as
// explicit convenience flags for existing adapter callers; Status is the
// canonical outcome.
type Result struct {
	Protocol    Protocol
	Status      Status
	OK          bool
	Unconfirmed bool
	Class       ErrClass
	StatusCode  int
	Latency     time.Duration
	Attempts    int
	RequestID   string // SHA-256-derived display token, never the raw header
	Detail      string // stable summary; response bodies are never included
}

// RelayResult applies the B12 relay-wide gate: at least one semantic success
// is required for each request protocol. Models inventory success does not
// substitute for either protocol.
type RelayResult struct {
	Status             Status
	Unconfirmed        bool
	ResponsesConfirmed bool
	MessagesConfirmed  bool
}

// EvaluateRelay computes the relay-wide outcome without inferring success from
// failures, model inventory, or agreement between failed endpoints.
func EvaluateRelay(results ...Result) RelayResult {
	verdict := RelayResult{Status: StatusUnconfirmed, Unconfirmed: true}
	for _, result := range results {
		if result.Status != StatusConfirmed {
			continue
		}
		switch result.Protocol {
		case ProtocolResponses:
			verdict.ResponsesConfirmed = true
		case ProtocolMessages:
			verdict.MessagesConfirmed = true
		}
	}
	if verdict.ResponsesConfirmed && verdict.MessagesConfirmed {
		verdict.Status = StatusConfirmed
		verdict.Unconfirmed = false
	}
	return verdict
}

// Prober is the adapter-facing protocol probe contract retained by the
// existing skeleton. Concrete HTTP probes below additionally accept the
// runtime model required by their protocol.
type Prober interface {
	Probe(ctx context.Context, p Protocol) (Result, error)
}

// BackoffPolicy bounds all retry behavior. MaxAttempts includes the initial
// request and may never exceed three.
type BackoffPolicy struct {
	MaxAttempts   int
	BaseDelay     time.Duration
	MaxRetryAfter time.Duration
	TotalBudget   time.Duration
}

// DefaultBackoff is deliberately small and bounded for an interactive
// installer while still honoring a capped Retry-After response.
func DefaultBackoff() BackoffPolicy {
	return BackoffPolicy{
		MaxAttempts:   3,
		BaseDelay:     time.Second,
		MaxRetryAfter: 30 * time.Second,
		TotalBudget:   60 * time.Second,
	}
}

func confirmedResult(protocol Protocol) Result {
	return Result{
		Protocol: protocol,
		Status:   StatusConfirmed,
		OK:       true,
		Class:    ClassNone,
	}
}

func unconfirmedResult(protocol Protocol, class ErrClass, detail string) Result {
	return Result{
		Protocol:    protocol,
		Status:      StatusUnconfirmed,
		Unconfirmed: true,
		Class:       class,
		Detail:      detail,
	}
}
