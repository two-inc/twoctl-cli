package httpx

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryOnTransientFailure(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	client := NewWithOptions(Options{APIKey: "k", MaxRetries: 3, Timeout: 5 * time.Second})
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("final status = %d, want 200", resp.StatusCode)
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Errorf("expected 3 attempts (2 transient + 1 success), got %d", attempts)
	}
}

func TestRetryGivesUp(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	client := NewWithOptions(Options{APIKey: "k", MaxRetries: 2, Timeout: 5 * time.Second})
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&attempts) != 3 {
		t.Errorf("expected MaxRetries+1 = 3 attempts, got %d", attempts)
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	client := NewWithOptions(Options{APIKey: "k", MaxRetries: 3, Timeout: 5 * time.Second})
	resp, _ := client.Get(srv.URL)
	if resp != nil {
		resp.Body.Close()
	}
	if atomic.LoadInt32(&attempts) != 1 {
		t.Errorf("4xx should not retry, got %d attempts", attempts)
	}
}

func TestIdempotencyKeyOnPOST(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New("k")
	resp, err := client.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(keys) != 1 || keys[0] == "" {
		t.Errorf("idempotency key missing: %v", keys)
	}
	if !strings.HasPrefix(keys[0], "twoctl-") {
		t.Errorf("idempotency key wrong shape: %q", keys[0])
	}
}

func TestNoIdempotencyKeyOnGET(t *testing.T) {
	var key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New("k")
	resp, _ := client.Get(srv.URL)
	if resp != nil {
		resp.Body.Close()
	}
	if key != "" {
		t.Errorf("GET should not carry Idempotency-Key, got %q", key)
	}
}

func TestRetryReplaysRequestBody(t *testing.T) {
	var attempts int32
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastBody = string(body)
		if atomic.AddInt32(&attempts, 1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewWithOptions(Options{APIKey: "k", MaxRetries: 3})
	resp, err := client.Post(srv.URL, "application/json", bytes.NewReader([]byte(`{"v":1}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if lastBody != `{"v":1}` {
		t.Errorf("body not replayed correctly on retry: %q", lastBody)
	}
}

func TestRetryAfterHeaderHonoured(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewWithOptions(Options{APIKey: "k", MaxRetries: 1, Timeout: 5 * time.Second})
	start := time.Now()
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&attempts) != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
	// Should be very fast since Retry-After: 0.
	if time.Since(start) > 2*time.Second {
		t.Errorf("unexpectedly slow with Retry-After:0")
	}
}

func TestContextCancellationStopsRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := NewWithOptions(Options{APIKey: "k", MaxRetries: 10})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if _, err := client.Do(req); err == nil {
		t.Error("expected context cancellation error")
	}
}

func TestDisableRetriesOverridesDefaultAndExplicitCount(t *testing.T) {
	for _, retries := range []int{0, 3} {
		client := NewWithOptions(Options{DisableRetries: true, MaxRetries: retries})
		if got := client.Transport.(*transport).maxRetries; got != 0 {
			t.Errorf("DisableRetries with MaxRetries=%d: got %d retries", retries, got)
		}
	}
}
