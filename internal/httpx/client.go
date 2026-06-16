// Package httpx provides the shared HTTP client used by every operation
// command. It injects auth, identity, an idempotency key on mutations, and
// retries on transient failures (429 and 5xx).
package httpx

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Version is overwritten at build time by the release pipeline.
var Version = "dev"

// Defaults for the shared client. The CLI exposes flags to override them
// per invocation.
const (
	DefaultTimeout    = 60 * time.Second
	DefaultMaxRetries = 3
	baseBackoff       = 250 * time.Millisecond
	maxBackoff        = 8 * time.Second
)

// Options controls the shared client behaviour. Zero values pick sensible
// defaults so callers can pass `Options{APIKey: k}` and ignore the rest.
type Options struct {
	APIKey     string
	Timeout    time.Duration
	MaxRetries int
}

// New returns an *http.Client wired with auth + identity headers, idempotency
// keys on non-idempotent methods, and retry-on-transient-failure semantics.
func New(apiKey string) *http.Client {
	return NewWithOptions(Options{APIKey: apiKey})
}

// NewWithOptions builds a client for callers that need to tune retries or
// timeout. Intended for tests that want a tighter loop.
func NewWithOptions(opt Options) *http.Client {
	if opt.Timeout == 0 {
		opt.Timeout = DefaultTimeout
	}
	if opt.MaxRetries == 0 {
		opt.MaxRetries = DefaultMaxRetries
	}
	return &http.Client{
		Timeout: opt.Timeout,
		Transport: &transport{
			apiKey:     opt.APIKey,
			ua:         fmt.Sprintf("twoctl/%s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH),
			base:       http.DefaultTransport,
			maxRetries: opt.MaxRetries,
		},
	}
}

type transport struct {
	apiKey     string
	ua         string
	base       http.RoundTripper
	maxRetries int
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone so the caller's request object isn't mutated, and pre-read the
	// body so we can replay it on retry. Empty/nil bodies are a no-op.
	body, err := drainBody(req)
	if err != nil {
		return nil, err
	}
	t.injectHeaders(req)

	var lastResp *http.Response
	var lastErr error
	for attempt := 0; attempt <= t.maxRetries; attempt++ {
		// Each attempt gets a fresh clone so transport-level state from the
		// previous attempt doesn't leak. Only the clone gets the replayed
		// body — leave the caller's req.Body untouched (callers may
		// introspect their original request after Do returns).
		attemptReq := req.Clone(req.Context())
		if body != nil {
			attemptReq.Body = io.NopCloser(bytes.NewReader(body))
		}
		resp, err := t.base.RoundTrip(attemptReq)
		lastResp, lastErr = resp, err

		if !shouldRetry(resp, err) || attempt == t.maxRetries {
			return resp, err
		}
		// Drain + close so the connection can be re-used.
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		select {
		case <-time.After(backoffDelay(attempt, resp)):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return lastResp, lastErr
}

func (t *transport) injectHeaders(req *http.Request) {
	if req.Header.Get("X-Api-Key") == "" && t.apiKey != "" {
		req.Header.Set("X-Api-Key", t.apiKey)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", t.ua)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if needsIdempotencyKey(req.Method) && req.Header.Get("Idempotency-Key") == "" {
		req.Header.Set("Idempotency-Key", newIdempotencyKey())
	}
}

// drainBody buffers the request body (if any) so it can be replayed on retry.
// http.Request.Body is single-use, so without this any retry would send an
// empty body to the server.
func drainBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return body, nil
}

func needsIdempotencyKey(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func newIdempotencyKey() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		// crypto/rand failing means the OS RNG is broken. A predictable
		// fallback would let an attacker who can observe one request
		// predict the next key (or pre-poison the server-side idempotency
		// cache). Panicking is the safe choice: we never send a request
		// with a guessable key.
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	return "twoctl-" + hex.EncodeToString(b[:])
}

// shouldRetry returns true for transient transport failures and for HTTP
// status codes that the API contract allows us to replay safely.
func shouldRetry(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return false
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return true
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		return true
	}
	return false
}

// backoffDelay applies exponential backoff with jitter so a thundering herd
// doesn't synchronise, and treats `Retry-After` as a lower bound rather than an
// override: the server hint is honoured only when it exceeds our own backoff,
// and is clamped to maxBackoff. This keeps `Retry-After: 0` (e.g. a fleet-wide
// rate-limit reset) from collapsing into a zero-delay retry storm, and stops a
// hostile or misconfigured `Retry-After: 999999` from parking the CLI for days.
// Mirrors twoadm-cli internal/httpx (see INF-1314).
func backoffDelay(attempt int, resp *http.Response) time.Duration {
	d := jitteredBackoff(attempt)
	if hint := retryAfter(resp); hint > d {
		d = hint
	}
	return d
}

// jitteredBackoff returns the exponential backoff for `attempt` (0-based) with
// [0, 25%) additive jitter, capped at maxBackoff.
func jitteredBackoff(attempt int) time.Duration {
	// Clamp attempt so the shift below can't overflow time.Duration on
	// pathological MaxRetries values. baseBackoff << 6 = 16s, already
	// past the cap, so any attempt ≥ 6 lands on maxBackoff anyway.
	if attempt > 6 {
		attempt = 6
	}
	d := baseBackoff << attempt
	if d > maxBackoff {
		d = maxBackoff
	}
	jitter := int64(d / 4)
	if jitter <= 0 {
		jitter = 1
	}
	return d + time.Duration(rand.Int64N(jitter))
}

// retryAfter parses the `Retry-After` header (seconds form), clamped to
// maxBackoff. Returns 0 when the header is absent, negative, or unparseable so
// the caller falls back to jittered exponential backoff.
func retryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0
	}
	d := time.Duration(secs) * time.Second
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}
