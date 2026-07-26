package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"relay-install/internal/secret"
)

const (
	// UserAgent is fixed so probes are identifiable without exposing local
	// environment details.
	UserAgent              = "relay-install/1"
	defaultMaxResponseBody = int64(1 << 20)
)

var (
	ErrCrossOriginRedirect = errors.New("probe: cross-origin redirect refused")
	ErrTooManyRedirects    = errors.New("probe: too many redirects")
)

// Clock makes retry timing deterministic in tests.
type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Option configures testable transport and timing boundaries. The user agent,
// redirect policy, and HTTPS requirement are intentionally not configurable.
type Option func(*clientOptions)

type clientOptions struct {
	httpClient      *http.Client
	clock           Clock
	backoff         BackoffPolicy
	maxResponseBody int64
}

// WithHTTPClient supplies a transport (typically an httptest TLS client). The
// client is copied before the redirect policy is installed.
func WithHTTPClient(client *http.Client) Option {
	return func(options *clientOptions) { options.httpClient = client }
}

// WithClock supplies the clock used for both elapsed-budget accounting and
// retry sleeps.
func WithClock(clock Clock) Option {
	return func(options *clientOptions) { options.clock = clock }
}

// WithBackoff supplies an explicit bounded retry policy.
func WithBackoff(policy BackoffPolicy) Option {
	return func(options *clientOptions) { options.backoff = policy }
}

// WithMaxResponseBody changes the in-memory response cap, primarily for
// focused tests.
func WithMaxResponseBody(limit int64) Option {
	return func(options *clientOptions) { options.maxResponseBody = limit }
}

// Client is the shared, policy-enforcing HTTP layer for all three probes.
type Client struct {
	baseURL         *url.URL
	key             secret.Key
	httpClient      *http.Client
	clock           Clock
	backoff         BackoffPolicy
	maxResponseBody int64
}

// NewClient validates and normalizes baseURL and installs the non-bypassable
// redirect policy. Accepted paths are the HTTPS origin root and an optional
// /v1 suffix; endpoint construction always produces exactly one /v1 segment.
func NewClient(baseURL string, key secret.Key, opts ...Option) (*Client, error) {
	normalized, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if key.Ref().Len == 0 {
		return nil, errors.New("probe: credential is required")
	}

	options := clientOptions{
		httpClient:      http.DefaultClient,
		clock:           systemClock{},
		backoff:         DefaultBackoff(),
		maxResponseBody: defaultMaxResponseBody,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if options.httpClient == nil {
		return nil, errors.New("probe: HTTP client is required")
	}
	if options.clock == nil {
		return nil, errors.New("probe: clock is required")
	}
	if err := validateBackoff(options.backoff); err != nil {
		return nil, err
	}
	if options.maxResponseBody <= 0 {
		return nil, errors.New("probe: response body limit must be positive")
	}

	httpClient := *options.httpClient
	httpClient.CheckRedirect = sameOriginRedirect
	return &Client{
		baseURL:         normalized,
		key:             key,
		httpClient:      &httpClient,
		clock:           options.clock,
		backoff:         options.backoff,
		maxResponseBody: options.maxResponseBody,
	}, nil
}

// NormalizeBaseURL returns the canonical HTTPS origin root. It accepts a
// trailing slash or /v1 suffix because both are common relay configuration
// forms, but rejects all other paths and URL metadata.
func NormalizeBaseURL(raw string) (string, error) {
	normalized, err := normalizeBaseURL(raw)
	if err != nil {
		return "", err
	}
	return normalized.String(), nil
}

func normalizeBaseURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) != raw || raw == "" {
		return nil, errors.New("probe: base URL must be a non-empty canonical URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("probe: invalid base URL")
	}
	if !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
		return nil, errors.New("probe: base URL must be an HTTPS origin")
	}
	if parsed.User != nil {
		return nil, errors.New("probe: base URL must not contain userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errors.New("probe: base URL must not contain query or fragment")
	}
	if parsed.Opaque != "" || parsed.RawPath != "" {
		return nil, errors.New("probe: base URL must not use opaque or encoded paths")
	}
	if parsed.Hostname() == "" {
		return nil, errors.New("probe: base URL must contain a host")
	}
	switch parsed.Path {
	case "", "/", "/v1", "/v1/":
	default:
		return nil, errors.New("probe: base URL path must be root or /v1")
	}
	parsed.Scheme = "https"
	parsed.Path = ""
	return parsed, nil
}

func validateBackoff(policy BackoffPolicy) error {
	if policy.MaxAttempts < 1 || policy.MaxAttempts > 3 {
		return errors.New("probe: max attempts must be between 1 and 3")
	}
	if policy.BaseDelay < 0 || policy.MaxRetryAfter < 0 {
		return errors.New("probe: backoff durations must not be negative")
	}
	if policy.TotalBudget <= 0 {
		return errors.New("probe: total budget must be positive")
	}
	return nil
}

func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if !sameOrigin(via[0].URL, req.URL) {
		return ErrCrossOriginRedirect
	}
	if len(via) >= 10 {
		return ErrTooManyRedirects
	}
	return nil
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) &&
		effectivePort(left) == effectivePort(right)
}

func effectivePort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

type requestSpec struct {
	protocol Protocol
	method   string
	path     string
	body     []byte
	headers  http.Header
	validate func([]byte) bool
}

func (client *Client) execute(ctx context.Context, spec requestSpec) (Result, []byte, error) {
	start := client.clock.Now()
	probeCtx, cancel := context.WithTimeout(ctx, client.backoff.TotalBudget)
	defer cancel()

	result := unconfirmedResult(spec.protocol, ClassTransient, "probe did not run")
	var lastRequestID string

	for attempt := 1; attempt <= client.backoff.MaxAttempts; attempt++ {
		if client.clock.Now().Sub(start) >= client.backoff.TotalBudget {
			result.Detail = "probe time budget exhausted"
			break
		}

		request, err := client.newRequest(probeCtx, spec)
		if err != nil {
			return Result{}, nil, err
		}
		response, err := client.httpClient.Do(request)
		if err != nil {
			result = unconfirmedResult(spec.protocol, classifyTransportError(err), transportDetail(err))
			result.Attempts = attempt
			if response != nil {
				result.StatusCode = response.StatusCode
				if requestID := redactRequestID(response.Header); requestID != "" {
					lastRequestID = requestID
				}
				result.RequestID = lastRequestID
				if response.Body != nil {
					response.Body.Close()
				}
			}
			if result.Class != ClassBackoff || attempt == client.backoff.MaxAttempts {
				break
			}
			if !client.waitForRetry(probeCtx, start, attempt, "") {
				result.Detail = "probe time budget exhausted"
				break
			}
			continue
		}

		requestID := redactRequestID(response.Header)
		if requestID != "" {
			lastRequestID = requestID
		}
		statusCode := response.StatusCode
		if statusCode < 200 || statusCode >= 300 {
			response.Body.Close()
			class, detail := classifyStatus(statusCode)
			result = unconfirmedResult(spec.protocol, class, detail)
			result.StatusCode = statusCode
			result.Attempts = attempt
			result.RequestID = lastRequestID
			if class != ClassBackoff || attempt == client.backoff.MaxAttempts {
				break
			}
			if !client.waitForRetry(probeCtx, start, attempt, response.Header.Get("Retry-After")) {
				result.Detail = "probe time budget exhausted"
				break
			}
			continue
		}

		body, tooLarge, readErr := readBounded(response.Body, client.maxResponseBody)
		response.Body.Close()
		if readErr != nil {
			result = unconfirmedResult(spec.protocol, classifyTransportError(readErr), transportDetail(readErr))
			result.StatusCode = statusCode
			result.Attempts = attempt
			result.RequestID = lastRequestID
			if result.Class == ClassBackoff && attempt < client.backoff.MaxAttempts &&
				client.waitForRetry(probeCtx, start, attempt, "") {
				continue
			}
			break
		}
		if tooLarge {
			result = unconfirmedResult(spec.protocol, ClassProtocol, "response body exceeds limit")
			result.StatusCode = statusCode
			result.Attempts = attempt
			result.RequestID = lastRequestID
			break
		}
		if spec.validate == nil || !spec.validate(body) {
			result = unconfirmedResult(spec.protocol, ClassProtocol, "response does not match protocol shape")
			result.StatusCode = statusCode
			result.Attempts = attempt
			result.RequestID = lastRequestID
			break
		}

		result = confirmedResult(spec.protocol)
		result.StatusCode = statusCode
		result.Attempts = attempt
		result.RequestID = lastRequestID
		result.Latency = nonNegativeDuration(client.clock.Now().Sub(start))
		return result, body, nil
	}

	result.RequestID = lastRequestID
	result.Latency = nonNegativeDuration(client.clock.Now().Sub(start))
	return result, nil, nil
}

func (client *Client) newRequest(ctx context.Context, spec requestSpec) (*http.Request, error) {
	endpoint := *client.baseURL
	endpoint.Path = spec.path
	request, err := http.NewRequestWithContext(ctx, spec.method, endpoint.String(), bytes.NewReader(spec.body))
	if err != nil {
		return nil, errors.New("probe: could not construct request")
	}
	request.Header.Set("User-Agent", UserAgent)
	request.Header.Set("Accept", "application/json")
	if len(spec.body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, values := range spec.headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	client.key.Reveal(func(plaintext string) {
		request.Header.Set("Authorization", "Bearer "+plaintext)
	})
	return request, nil
}

func (client *Client) waitForRetry(ctx context.Context, start time.Time, attempt int, retryAfter string) bool {
	delay := exponentialDelay(client.backoff.BaseDelay, attempt)
	if headerDelay, ok := parseRetryAfter(retryAfter, client.clock.Now(), client.backoff.MaxRetryAfter); ok && headerDelay > delay {
		delay = headerDelay
	}
	remaining := client.backoff.TotalBudget - client.clock.Now().Sub(start)
	if remaining <= 0 || delay >= remaining {
		return false
	}
	return client.clock.Sleep(ctx, delay) == nil
}

func exponentialDelay(base time.Duration, attempt int) time.Duration {
	delay := base
	for multiplier := 1; multiplier < attempt; multiplier++ {
		if delay > time.Duration(1<<63-1)/2 {
			return time.Duration(1<<63 - 1)
		}
		delay *= 2
	}
	return delay
}

func parseRetryAfter(raw string, now time.Time, cap time.Duration) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
		if seconds > int64(cap/time.Second) {
			return cap, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if isDecimal(raw) {
		// An overflowing delta-seconds value is still a valid request for a very
		// long delay. Honor it at the configured cap.
		return cap, true
	}
	when, err := http.ParseTime(raw)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		delay = 0
	}
	if delay > cap {
		delay = cap
	}
	return delay, true
}

func classifyStatus(status int) (ErrClass, string) {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ClassCredential, "credential rejected"
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return ClassProtocol, "protocol endpoint unsupported"
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable:
		return ClassBackoff, "relay temporarily unavailable"
	default:
		return ClassProtocol, fmt.Sprintf("unexpected HTTP status %d", status)
	}
}

func classifyTransportError(err error) ErrClass {
	if errors.Is(err, ErrCrossOriginRedirect) || errors.Is(err, ErrTooManyRedirects) {
		return ClassProtocol
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ClassBackoff
	}
	if errors.Is(err, context.Canceled) {
		return ClassTransient
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return ClassBackoff
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return ClassBackoff
	}
	var recordHeaderError tls.RecordHeaderError
	if errors.As(err, &recordHeaderError) {
		return ClassBackoff
	}
	var certificateVerificationError *tls.CertificateVerificationError
	if errors.As(err, &certificateVerificationError) {
		return ClassBackoff
	}
	var unknownAuthorityError x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthorityError) {
		return ClassBackoff
	}
	var hostnameError x509.HostnameError
	if errors.As(err, &hostnameError) {
		return ClassBackoff
	}
	var certificateInvalidError x509.CertificateInvalidError
	if errors.As(err, &certificateInvalidError) {
		return ClassBackoff
	}
	var systemRootsError *x509.SystemRootsError
	if errors.As(err, &systemRootsError) {
		return ClassBackoff
	}
	var alertError tls.AlertError
	if errors.As(err, &alertError) {
		return ClassBackoff
	}
	return ClassTransient
}

func transportDetail(err error) string {
	if errors.Is(err, ErrCrossOriginRedirect) {
		return "cross-origin redirect refused"
	}
	if errors.Is(err, ErrTooManyRedirects) {
		return "redirect limit exceeded"
	}
	switch classifyTransportError(err) {
	case ClassBackoff:
		return "retryable transport failure"
	default:
		return "transport failure"
	}
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func readBounded(reader io.Reader, limit int64) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > limit {
		return nil, true, nil
	}
	return body, false, nil
}

func redactRequestID(header http.Header) string {
	var raw string
	for _, name := range []string{"X-Request-ID", "Request-ID"} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			raw = value
			break
		}
	}
	if raw == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(raw))
	return "sha256:" + hex.EncodeToString(digest[:6])
}

func nonNegativeDuration(duration time.Duration) time.Duration {
	if duration < 0 {
		return 0
	}
	return duration
}
