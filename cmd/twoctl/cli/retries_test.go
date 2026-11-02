package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestMaxRetriesAppliesToEveryAPIPath(t *testing.T) {
	for _, paginated := range []bool{false, true} {
		for _, retries := range []int{0, 1, -1} {
			t.Run(fmt.Sprintf("paginated=%t/retries=%d", paginated, retries), func(t *testing.T) {
				checkRetryAttempts(t, paginated, retries)
			})
		}
	}
}

// A negative value here means omit the flag and assert the default policy.
func checkRetryAttempts(t *testing.T, paginated bool, retries int) {
	t.Helper()
	resetGlobals(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"temporary"}`)
	}))
	defer srv.Close()
	args := []string{"--url", srv.URL, "--api-key", "secret_test_x", "billing-statement", "get"}
	if paginated {
		args = append(args, "--all")
	}
	want := 4
	if retries >= 0 {
		args = append(args, "--max-retries", strconv.Itoa(retries))
		want = retries + 1
	}
	_, _, err := runRoot(t, args...)
	ae, ok := err.(*apiError)
	if !ok || ae.status != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP 503 apiError, got %v", err)
	}
	if calls != want {
		t.Fatalf("got %d HTTP attempts, want %d", calls, want)
	}
}

func TestNegativeMaxRetriesDoesNotSendRequest(t *testing.T) {
	for _, paginated := range []bool{false, true} {
		t.Run(fmt.Sprintf("paginated=%t", paginated), func(t *testing.T) {
			resetGlobals(t)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
			}))
			defer srv.Close()
			args := []string{"--url", srv.URL, "--api-key", "secret_test_x", "--max-retries", "-1", "billing-statement", "get"}
			if paginated {
				args = append(args, "--all")
			}
			_, _, err := runRoot(t, args...)
			ae, ok := err.(*apiError)
			if !ok || ae.code != "invalid_max_retries" || ae.exit != ExitUsage {
				t.Fatalf("expected usage error, got %v", err)
			}
			if calls != 0 {
				t.Fatalf("sent %d requests with invalid retry setting", calls)
			}
		})
	}
}

func TestMaxRetriesAppliesToEveryPage(t *testing.T) {
	resetGlobals(t)
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("page_cursor")
		calls[cursor]++
		if calls[cursor] == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if cursor == "" {
			fmt.Fprint(w, `{"items":[{"id":"a"}],"next_page_cursor":"second"}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"id":"b"}]}`)
	}))
	defer srv.Close()
	_, _, err := runRoot(t, "--url", srv.URL, "--api-key", "secret_test_x",
		"billing-statement", "get", "--all", "--max-retries", "1")
	if err != nil {
		t.Fatal(err)
	}
	if calls[""] != 2 || calls["second"] != 2 || len(calls) != 2 {
		t.Fatalf("expected one retry for each page, got %v", calls)
	}
}
