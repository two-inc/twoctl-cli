package httpx

import (
	"net/http"
	"testing"
	"time"
)

func TestBackoffDelayClampsAttempt(t *testing.T) {
	// Pathological attempt values should not panic and should clamp at
	// maxBackoff (+ jitter).
	for _, attempt := range []int{0, 5, 10, 50, 100, 1000} {
		d := backoffDelay(attempt, nil)
		if d <= 0 {
			t.Errorf("attempt=%d returned non-positive delay %v", attempt, d)
		}
		if d > maxBackoff+maxBackoff/4 {
			t.Errorf("attempt=%d returned %v which exceeds the cap+jitter window", attempt, d)
		}
	}
}

func TestBackoffDelayHonoursRetryAfter(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"7"}}}
	if d := backoffDelay(0, resp); d != 7*time.Second {
		t.Errorf("Retry-After: 7 → %v, want 7s", d)
	}
}

func TestBackoffDelayBadRetryAfterFallsBack(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"not a number"}}}
	d := backoffDelay(0, resp)
	if d <= 0 {
		t.Errorf("garbage Retry-After should fall back to backoff, got %v", d)
	}
}

func TestBackoffDelayClampsLargeRetryAfter(t *testing.T) {
	// A hostile or misconfigured upstream must not be able to park the CLI for
	// days: the hint is clamped to maxBackoff.
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"999999"}}}
	if d := backoffDelay(0, resp); d != maxBackoff {
		t.Errorf("Retry-After: 999999 → %v, want clamp to %v", d, maxBackoff)
	}
}

func TestBackoffDelayFloorsZeroRetryAfter(t *testing.T) {
	// Retry-After: 0 (e.g. a fleet-wide rate-limit reset) must not collapse
	// into a zero-delay retry storm; it floors at the jittered backoff.
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"0"}}}
	d := backoffDelay(0, resp)
	if d < baseBackoff {
		t.Errorf("Retry-After: 0 → %v, want at least baseBackoff %v", d, baseBackoff)
	}
	if d > maxBackoff+maxBackoff/4 {
		t.Errorf("Retry-After: 0 → %v exceeds cap+jitter window", d)
	}
}

func TestBackoffDelayRetryAfterIsLowerBoundNotCeiling(t *testing.T) {
	// At a high attempt our own backoff already sits at the cap; a small
	// Retry-After must not shrink it below that.
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"1"}}}
	if d := backoffDelay(6, resp); d < maxBackoff {
		t.Errorf("backoff at attempt 6 should be >= %v despite Retry-After: 1, got %v", maxBackoff, d)
	}
}
