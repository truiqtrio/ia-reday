package probe

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"relay-install/internal/secret"
)

const testKeyValue = "probe-canary-key-not-for-output"

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)}
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Sleep(ctx context.Context, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.sleeps = append(clock.sleeps, delay)
	clock.now = clock.now.Add(delay)
	return nil
}

func (clock *fakeClock) Sleeps() []time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return append([]time.Duration(nil), clock.sleeps...)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type nonTimeoutNetError struct{}

func (nonTimeoutNetError) Error() string   { return "deterministic client error" }
func (nonTimeoutNetError) Timeout() bool   { return false }
func (nonTimeoutNetError) Temporary() bool { return false }

type handlerRoundTripper struct{ handler http.Handler }

func (transport handlerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	transport.handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	response.Request = request
	return response, nil
}

func mustKey(t *testing.T) secret.Key {
	t.Helper()
	key, err := secret.New("test", testKeyValue)
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	return key
}

func newProbeClient(t *testing.T, handler http.Handler, opts ...Option) *Client {
	t.Helper()
	httpClient := &http.Client{Transport: handlerRoundTripper{handler: handler}}
	allOpts := []Option{WithHTTPClient(httpClient)}
	allOpts = append(allOpts, opts...)
	client, err := NewClient("https://relay.test", mustKey(t), allOpts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func successfulBody(protocol Protocol) string {
	switch protocol {
	case ProtocolModels:
		return `{"object":"list","data":[{"id":"gpt-5.6-sol","object":"model"},{"id":"claude-opus-5","object":"model"},{"id":"glm-5.2","object":"model"}]}`
	case ProtocolResponses:
		return `{"id":"resp_123","object":"response","status":"completed","model":"runtime-model","error":null,"output":[{"id":"msg_123","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`
	case ProtocolMessages:
		return `{"id":"msg_123","type":"message","role":"assistant","model":"runtime-model","stop_reason":"max_tokens","content":[{"type":"text","text":"o"}]}`
	default:
		panic("unsupported protocol")
	}
}

func invalidBody(protocol Protocol) string {
	switch protocol {
	case ProtocolModels:
		return `{"object":"list","data":[]}`
	case ProtocolResponses:
		return `{"id":"chat_123","object":"chat.completion","choices":[]}`
	case ProtocolMessages:
		return `{"id":"msg_123","type":"message","role":"assistant","content":{}}`
	default:
		panic("unsupported protocol")
	}
}

func callProtocol(ctx context.Context, t *testing.T, client *Client, protocol Protocol) (Result, error) {
	t.Helper()
	switch protocol {
	case ProtocolModels:
		result, err := NewModelsProbe(client).Probe(ctx)
		return result.Result, err
	case ProtocolResponses:
		return NewResponsesProbe(client).Probe(ctx, "runtime-model")
	case ProtocolMessages:
		return NewMessagesProbe(client).Probe(ctx, "runtime-model")
	default:
		t.Fatalf("unsupported protocol %q", protocol)
		return Result{}, nil
	}
}

func TestProbesSuccessAndRequestShape(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolModels, ProtocolResponses, ProtocolMessages} {
		t.Run(string(protocol), func(t *testing.T) {
			client := newProbeClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v1/"+string(protocol) {
					t.Errorf("path = %q", request.URL.Path)
				}
				wantMethod := http.MethodPost
				if protocol == ProtocolModels {
					wantMethod = http.MethodGet
				}
				if request.Method != wantMethod {
					t.Errorf("method = %q, want %q", request.Method, wantMethod)
				}
				if got := request.Header.Get("User-Agent"); got != UserAgent {
					t.Errorf("User-Agent = %q", got)
				}
				if got := request.Header.Get("Authorization"); got != "Bearer "+testKeyValue {
					t.Errorf("Authorization did not use the supplied key")
				}
				if protocol != ProtocolModels {
					var payload map[string]any
					if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
						t.Errorf("decode request: %v", err)
					}
					if payload["model"] != "runtime-model" || payload["stream"] != false {
						t.Errorf("runtime model/stream payload = %#v", payload)
					}
					if protocol == ProtocolResponses {
						if _, exists := payload["max_output_tokens"]; exists {
							t.Errorf("responses request is not minimal: %#v", payload)
						}
					}
					if protocol == ProtocolMessages && payload["max_tokens"] != float64(1) {
						t.Errorf("max_tokens = %#v", payload["max_tokens"])
					}
				}
				writer.Header().Set("X-Request-ID", "private-request-id")
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, successfulBody(protocol))
			}))

			result, err := callProtocol(context.Background(), t, client, protocol)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if result.Status != StatusConfirmed || !result.OK || result.Unconfirmed || result.Class != ClassNone {
				t.Fatalf("result = %+v", result)
			}
			if result.Attempts != 1 || result.StatusCode != http.StatusOK {
				t.Fatalf("attempt/status = %d/%d", result.Attempts, result.StatusCode)
			}
			if result.RequestID == "" || result.RequestID == "private-request-id" || strings.Contains(result.RequestID, "private") {
				t.Fatalf("request ID was not redacted: %q", result.RequestID)
			}
		})
	}
}

func TestProbesRejectInvalidSuccessShapeWithoutBodyDisclosure(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolModels, ProtocolResponses, ProtocolMessages} {
		t.Run(string(protocol), func(t *testing.T) {
			body := invalidBody(protocol) + testKeyValue
			client := newProbeClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(writer, body)
			}))
			result, err := callProtocol(context.Background(), t, client, protocol)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if result.Status != StatusUnconfirmed || result.OK || !result.Unconfirmed || result.Class != ClassProtocol {
				t.Fatalf("result = %+v", result)
			}
			if strings.Contains(result.Detail, testKeyValue) || strings.Contains(result.Detail, invalidBody(protocol)) {
				t.Fatalf("response body leaked into detail: %q", result.Detail)
			}
		})
	}
}

func TestFailedOrMalformedProtocolPayloadsRemainUnconfirmed(t *testing.T) {
	tests := []struct {
		name     string
		protocol Protocol
		body     string
	}{
		{"models-wrong-discriminator", ProtocolModels, `{"object":"catalog","data":[{"id":"gpt-5.6-sol","object":"model"}]}`},
		{"models-invalid-entry", ProtocolModels, `{"object":"list","data":[{"id":"gpt-5.6-sol","object":"unknown"}]}`},
		{"responses-failed", ProtocolResponses, `{"id":"resp_1","object":"response","status":"failed","model":"runtime-model","error":{"message":"no"},"output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"x"}]}]}`},
		{"responses-null-item", ProtocolResponses, `{"id":"resp_1","object":"response","status":"completed","model":"runtime-model","error":null,"output":[null]}`},
		{"responses-scalar-content", ProtocolResponses, `{"id":"resp_1","object":"response","status":"completed","model":"runtime-model","error":null,"output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[1]}]}`},
		{"messages-null-item", ProtocolMessages, `{"id":"msg_1","type":"message","role":"assistant","model":"runtime-model","stop_reason":"max_tokens","content":[null]}`},
		{"messages-scalar-item", ProtocolMessages, `{"id":"msg_1","type":"message","role":"assistant","model":"runtime-model","stop_reason":"max_tokens","content":[1]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newProbeClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(writer, test.body)
			}))
			result, err := callProtocol(context.Background(), t, client, test.protocol)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if result.Status != StatusUnconfirmed || result.Class != ClassProtocol || result.Attempts != 1 {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestModelsProbeReturnsInventoryAndGroups(t *testing.T) {
	client := newProbeClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"object":"list","data":[{"id":"gpt-5.6-sol","object":"model"},{"id":"claude-opus-5","object":"model"},{"id":"qwen3.5","object":"model"},{"id":"other-model","object":"model"}]}`)
	}))
	result, err := NewModelsProbe(client).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Status != StatusConfirmed || len(result.Models) != 4 {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Groups[GroupGPT]) != 1 || len(result.Groups[GroupAnthropic]) != 1 ||
		len(result.Groups[GroupChina]) != 1 || len(result.Groups[GroupUnknown]) != 1 {
		t.Fatalf("groups = %#v", result.Groups)
	}
}

func TestGroupClassify(t *testing.T) {
	tests := map[string]ModelGroup{
		"gpt-5.6-sol":       GroupGPT,
		"GPT-5.4":           GroupGPT,
		"claude-opus-5":     GroupAnthropic,
		"gemma4":            GroupChina,
		"glm-5.2":           GroupChina,
		"qwen3.5":           GroupChina,
		"kimi-k2.7-code":    GroupChina,
		"kimi-k2.6":         GroupChina,
		"minimax-m3":        GroupChina,
		"unlisted-model":    GroupUnknown,
		"":                  GroupUnknown,
		"  claude-sonnet-5": GroupAnthropic,
	}
	for model, want := range tests {
		if got := GroupClassify(model); got != want {
			t.Errorf("GroupClassify(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestEvaluateRelayRequiresBothSemanticProtocols(t *testing.T) {
	confirmedResponses := confirmedResult(ProtocolResponses)
	confirmedMessages := confirmedResult(ProtocolMessages)
	confirmedModels := confirmedResult(ProtocolModels)
	failedMessages := unconfirmedResult(ProtocolMessages, ClassCredential, "credential rejected")

	tests := []struct {
		name          string
		results       []Result
		wantStatus    Status
		wantResponses bool
		wantMessages  bool
	}{
		{"empty", nil, StatusUnconfirmed, false, false},
		{"models-only", []Result{confirmedModels}, StatusUnconfirmed, false, false},
		{"responses-only", []Result{confirmedResponses}, StatusUnconfirmed, true, false},
		{"messages-only", []Result{confirmedMessages}, StatusUnconfirmed, false, true},
		{"failed-messages-do-not-count", []Result{confirmedResponses, failedMessages}, StatusUnconfirmed, true, false},
		{"both", []Result{confirmedResponses, confirmedMessages}, StatusConfirmed, true, true},
		{"one-of-several-per-protocol", []Result{unconfirmedResult(ProtocolResponses, ClassBackoff, "temporary"), confirmedResponses, failedMessages, confirmedMessages}, StatusConfirmed, true, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := EvaluateRelay(test.results...)
			if got.Status != test.wantStatus || got.ResponsesConfirmed != test.wantResponses || got.MessagesConfirmed != test.wantMessages {
				t.Fatalf("EvaluateRelay = %+v", got)
			}
			if got.Unconfirmed != (test.wantStatus == StatusUnconfirmed) {
				t.Fatalf("Unconfirmed = %v for status %q", got.Unconfirmed, got.Status)
			}
		})
	}
}

func TestCredentialAndProtocolStatusesNeverRetry(t *testing.T) {
	tests := []struct {
		protocol Protocol
		status   int
		class    ErrClass
	}{
		{ProtocolModels, http.StatusUnauthorized, ClassCredential},
		{ProtocolModels, http.StatusForbidden, ClassCredential},
		{ProtocolResponses, http.StatusUnauthorized, ClassCredential},
		{ProtocolResponses, http.StatusForbidden, ClassCredential},
		{ProtocolMessages, http.StatusUnauthorized, ClassCredential},
		{ProtocolMessages, http.StatusForbidden, ClassCredential},
		{ProtocolModels, http.StatusNotFound, ClassProtocol},
		{ProtocolResponses, http.StatusMethodNotAllowed, ClassProtocol},
		{ProtocolMessages, http.StatusNotFound, ClassProtocol},
	}
	for _, test := range tests {
		name := string(test.protocol) + "/" + http.StatusText(test.status)
		t.Run(name, func(t *testing.T) {
			var attempts atomic.Int32
			client := newProbeClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, testKeyValue)
			}))
			result, err := callProtocol(context.Background(), t, client, test.protocol)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if result.Status != StatusUnconfirmed || result.Class != test.class || result.Attempts != 1 {
				t.Fatalf("result = %+v", result)
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("attempts = %d", got)
			}
			if strings.Contains(result.Detail, testKeyValue) {
				t.Fatalf("error body leaked into detail: %q", result.Detail)
			}
		})
	}
}

func TestRetryableStatusesUseBoundedDeterministicBackoff(t *testing.T) {
	tests := []struct {
		protocol   Protocol
		status     int
		retryAfter string
		wantSleeps []time.Duration
	}{
		{ProtocolModels, http.StatusTooManyRequests, "120", []time.Duration{5 * time.Second, 5 * time.Second}},
		{ProtocolResponses, http.StatusBadGateway, "", []time.Duration{time.Second, 2 * time.Second}},
		{ProtocolMessages, http.StatusServiceUnavailable, "", []time.Duration{time.Second, 2 * time.Second}},
	}
	for _, test := range tests {
		t.Run(string(test.protocol), func(t *testing.T) {
			clock := newFakeClock()
			var attempts atomic.Int32
			var authorizations []string
			var authMu sync.Mutex
			client := newProbeClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				attempts.Add(1)
				authMu.Lock()
				authorizations = append(authorizations, request.Header.Get("Authorization"))
				authMu.Unlock()
				if test.retryAfter != "" {
					writer.Header().Set("Retry-After", test.retryAfter)
				}
				writer.Header().Set("X-Request-ID", "retry-request-id")
				writer.WriteHeader(test.status)
			}),
				WithClock(clock),
				WithBackoff(BackoffPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxRetryAfter: 5 * time.Second, TotalBudget: 30 * time.Second}),
			)
			result, err := callProtocol(context.Background(), t, client, test.protocol)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if result.Status != StatusUnconfirmed || result.Class != ClassBackoff || result.Attempts != 3 {
				t.Fatalf("result = %+v", result)
			}
			if got := attempts.Load(); got != 3 {
				t.Fatalf("attempts = %d", got)
			}
			if got := clock.Sleeps(); !equalDurations(got, test.wantSleeps) {
				t.Fatalf("sleeps = %v, want %v", got, test.wantSleeps)
			}
			for _, authorization := range authorizations {
				if authorization != "Bearer "+testKeyValue {
					t.Fatalf("authorization changed between attempts")
				}
			}
			if result.RequestID == "" || strings.Contains(result.RequestID, "retry-request-id") {
				t.Fatalf("request ID was not redacted: %q", result.RequestID)
			}
		})
	}
}

func TestRetryCanRecoverToConfirmed(t *testing.T) {
	clock := newFakeClock()
	var attempts atomic.Int32
	client := newProbeClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(writer, successfulBody(ProtocolMessages))
	}),
		WithClock(clock),
		WithBackoff(BackoffPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxRetryAfter: 5 * time.Second, TotalBudget: 10 * time.Second}),
	)
	result, err := NewMessagesProbe(client).Probe(context.Background(), "owner-model")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Status != StatusConfirmed || result.Attempts != 2 || !equalDurations(clock.Sleeps(), []time.Duration{time.Second}) {
		t.Fatalf("result = %+v, sleeps = %v", result, clock.Sleeps())
	}
}

func TestRetryableTransportFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"timeout", context.DeadlineExceeded},
		{"dns", &net.DNSError{Err: "temporary", Name: "relay.invalid", IsTemporary: true}},
		{"tls", &tls.CertificateVerificationError{Err: errors.New("certificate rejected")}},
	}
	for _, test := range tests {
		for _, protocol := range []Protocol{ProtocolModels, ProtocolResponses, ProtocolMessages} {
			t.Run(test.name+"/"+string(protocol), func(t *testing.T) {
				clock := newFakeClock()
				var attempts atomic.Int32
				httpClient := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					attempts.Add(1)
					return nil, test.err
				})}
				client, err := NewClient("https://relay.invalid", mustKey(t),
					WithHTTPClient(httpClient),
					WithClock(clock),
					WithBackoff(BackoffPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxRetryAfter: 5 * time.Second, TotalBudget: 10 * time.Second}),
				)
				if err != nil {
					t.Fatalf("NewClient: %v", err)
				}
				result, err := callProtocol(context.Background(), t, client, protocol)
				if err != nil {
					t.Fatalf("Probe: %v", err)
				}
				if result.Status != StatusUnconfirmed || result.Class != ClassBackoff || result.Attempts != 3 {
					t.Fatalf("result = %+v", result)
				}
				if attempts.Load() != 3 || !equalDurations(clock.Sleeps(), []time.Duration{time.Second, 2 * time.Second}) {
					t.Fatalf("attempts/sleeps = %d/%v", attempts.Load(), clock.Sleeps())
				}
			})
		}
	}
}

func TestNonTimeoutNetErrorDoesNotRetry(t *testing.T) {
	clock := newFakeClock()
	var attempts atomic.Int32
	httpClient := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, nonTimeoutNetError{}
	})}
	client, err := NewClient("https://relay.invalid", mustKey(t),
		WithHTTPClient(httpClient),
		WithClock(clock),
		WithBackoff(BackoffPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxRetryAfter: 5 * time.Second, TotalBudget: 10 * time.Second}),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	result, err := NewResponsesProbe(client).Probe(context.Background(), "runtime-model")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Status != StatusUnconfirmed || result.Class != ClassTransient || result.Attempts != 1 || attempts.Load() != 1 {
		t.Fatalf("result/attempts = %+v/%d", result, attempts.Load())
	}
	if len(clock.Sleeps()) != 0 {
		t.Fatalf("unexpected sleeps: %v", clock.Sleeps())
	}
}

func TestTotalBudgetStopsBeforeExcessiveRetry(t *testing.T) {
	clock := newFakeClock()
	var attempts atomic.Int32
	httpClient := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, context.DeadlineExceeded
	})}
	client, err := NewClient("https://relay.invalid", mustKey(t),
		WithHTTPClient(httpClient),
		WithClock(clock),
		WithBackoff(BackoffPolicy{MaxAttempts: 3, BaseDelay: 4 * time.Second, MaxRetryAfter: 5 * time.Second, TotalBudget: 5 * time.Second}),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	result, err := NewResponsesProbe(client).Probe(context.Background(), "runtime-model")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if attempts.Load() != 2 || result.Attempts != 2 || !equalDurations(clock.Sleeps(), []time.Duration{4 * time.Second}) {
		t.Fatalf("result/attempts/sleeps = %+v/%d/%v", result, attempts.Load(), clock.Sleeps())
	}
}

func TestBaseURLNormalizationAndEndpointJoining(t *testing.T) {
	for _, raw := range []string{
		"https://relay.example",
		"https://relay.example/",
		"https://relay.example/v1",
		"https://relay.example/v1/",
	} {
		t.Run(raw, func(t *testing.T) {
			var gotURL string
			httpClient := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				gotURL = request.URL.String()
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(successfulBody(ProtocolModels))),
					Request:    request,
				}, nil
			})}
			client, err := NewClient(raw, mustKey(t), WithHTTPClient(httpClient))
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			result, err := NewModelsProbe(client).Probe(context.Background())
			if err != nil || result.Status != StatusConfirmed {
				t.Fatalf("Probe = %+v, %v", result, err)
			}
			if gotURL != "https://relay.example/v1/models" {
				t.Fatalf("URL = %q", gotURL)
			}
			normalized, err := NormalizeBaseURL(raw)
			if err != nil || normalized != "https://relay.example" {
				t.Fatalf("NormalizeBaseURL = %q, %v", normalized, err)
			}
		})
	}
}

func TestBaseURLRejectsUnsafeForms(t *testing.T) {
	for _, raw := range []string{
		"http://relay.example",
		"https://user:pass@relay.example",
		"https://relay.example?token=x",
		"https://relay.example/#fragment",
		"https://relay.example#",
		"https://relay.example/api",
		" https://relay.example",
		"",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := NewClient(raw, mustKey(t)); err == nil {
				t.Fatalf("NewClient(%q) succeeded", raw)
			}
		})
	}
	if _, err := NewClient("https://relay.example", secret.Key{}); err == nil {
		t.Fatal("NewClient accepted a zero-value key")
	}
}

func TestHTTPProberImplementsProtocolDispatch(t *testing.T) {
	client := newProbeClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		protocol := Protocol(strings.TrimPrefix(request.URL.Path, "/v1/"))
		_, _ = io.WriteString(writer, successfulBody(protocol))
	}))
	prober, err := NewHTTPProber(client, "owner-model")
	if err != nil {
		t.Fatalf("NewHTTPProber: %v", err)
	}
	for _, protocol := range []Protocol{ProtocolModels, ProtocolResponses, ProtocolMessages} {
		result, err := prober.Probe(context.Background(), protocol)
		if err != nil || result.Status != StatusConfirmed {
			t.Fatalf("Probe(%q) = %+v, %v", protocol, result, err)
		}
	}
	if _, err := prober.Probe(context.Background(), Protocol("unknown")); err == nil {
		t.Fatal("unsupported protocol succeeded")
	}
}

func TestCrossOriginRedirectIsRefused(t *testing.T) {
	var targetHits atomic.Int32
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		if request.URL.Host == "target.test" {
			targetHits.Add(1)
			_, _ = io.WriteString(recorder, successfulBody(ProtocolModels))
		} else {
			recorder.Header().Set("Location", "https://target.test/v1/models")
			recorder.WriteHeader(http.StatusFound)
		}
		response := recorder.Result()
		response.Request = request
		return response, nil
	})
	client, err := NewClient("https://source.test", mustKey(t), WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	result, err := NewModelsProbe(client).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Status != StatusUnconfirmed || result.Class != ClassProtocol || result.Attempts != 1 {
		t.Fatalf("result = %+v", result)
	}
	if targetHits.Load() != 0 {
		t.Fatalf("cross-origin target received %d requests", targetHits.Load())
	}
}

func TestRedirectLoopDoesNotStartOuterRetries(t *testing.T) {
	var requests atomic.Int32
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		recorder := httptest.NewRecorder()
		recorder.Header().Set("Location", "https://relay.test/v1/models")
		recorder.WriteHeader(http.StatusTemporaryRedirect)
		response := recorder.Result()
		response.Request = request
		return response, nil
	})
	client, err := NewClient("https://relay.test", mustKey(t), WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	result, err := NewModelsProbe(client).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Status != StatusUnconfirmed || result.Class != ClassProtocol || result.Attempts != 1 {
		t.Fatalf("result = %+v", result)
	}
	if got := requests.Load(); got != 10 {
		t.Fatalf("redirect requests = %d, want one default redirect chain (10)", got)
	}
}

func TestSameOriginRedirectIsAllowed(t *testing.T) {
	var redirectedHits atomic.Int32
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		if request.URL.Path == "/redirected-models" {
			redirectedHits.Add(1)
			_, _ = io.WriteString(recorder, successfulBody(ProtocolModels))
		} else {
			recorder.Header().Set("Location", "https://relay.test/redirected-models")
			recorder.WriteHeader(http.StatusTemporaryRedirect)
		}
		response := recorder.Result()
		response.Request = request
		return response, nil
	})
	client, err := NewClient("https://relay.test", mustKey(t), WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	result, err := NewModelsProbe(client).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Status != StatusConfirmed || redirectedHits.Load() != 1 {
		t.Fatalf("result/hits = %+v/%d", result, redirectedHits.Load())
	}
}

func TestResponseBodyLimit(t *testing.T) {
	client := newProbeClient(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, successfulBody(ProtocolResponses)+strings.Repeat("x", 64))
	}), WithMaxResponseBody(16))
	result, err := NewResponsesProbe(client).Probe(context.Background(), "runtime-model")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Status != StatusUnconfirmed || result.Class != ClassProtocol || result.Detail != "response body exceeds limit" {
		t.Fatalf("result = %+v", result)
	}
}

func TestOverflowingRetryAfterUsesCap(t *testing.T) {
	clock := newFakeClock()
	delay, ok := parseRetryAfter(strings.Repeat("9", 100), clock.Now(), 7*time.Second)
	if !ok || delay != 7*time.Second {
		t.Fatalf("parseRetryAfter = %v, %v", delay, ok)
	}
}

func equalDurations(left, right []time.Duration) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
