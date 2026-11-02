package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestDiagnosticVerbosityAndRedaction(t *testing.T) {
	for _, level := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("level=%d", level), func(t *testing.T) {
			checkDiagnosticVerbosity(t, level)
		})
	}
}

func checkDiagnosticVerbosity(t *testing.T, level int) {
	t.Helper()
	resetGlobals(t)
	t.Setenv("TWOCTL_SKIP_UPGRADE_CHECK", "1")
	const payload = `{"password":"request-private","note":"secret_test_abcdefghijklmnop","amount":42}`
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		sent = string(raw)
		w.Header().Set("X-Request-Id", "request-123")
		w.Header().Set("Set-Cookie", "session=response-private")
		fmt.Fprint(w, `{"token":"response-private","payment_link":"https://checkout.two.inc/payment/initiation?token=abc123","id":9007199254740993}`)
	}))
	defer srv.Close()
	args := []string{"--url", srv.URL, "--api-key", "api-private", "order", "create", "--data", payload}
	if level > 0 {
		args = append(args, "-"+strings.Repeat("v", level))
	}
	stdout, stderr, err := runRoot(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	if sent != payload || !strings.Contains(stdout, "response-private") {
		t.Fatalf("diagnostics changed the request or stdout: body=%s stdout=%s", sent, stdout)
	}
	if level == 0 {
		if stderr != "" {
			t.Fatalf("default request produced diagnostics: %s", stderr)
		}
		return
	}
	record := decodeDiagnostic(t, stderr)
	if record["request_id"] != "request-123" || record["status"] != json.Number("200") || record["duration_ms"] == nil {
		t.Fatalf("missing request summary: %v", record)
	}
	if level == 1 {
		if _, exists := record["request_body"]; exists {
			t.Fatal("-v included body details")
		}
		return
	}
	assertNoDiagnosticSecrets(t, stderr)
	response := record["response_body"].(map[string]any)
	if response["id"] != json.Number("9007199254740993") {
		t.Fatalf("diagnostics lost integer precision: %v", response)
	}
	if record["request_body"].(map[string]any)["amount"] != json.Number("42") {
		t.Fatal("diagnostics omitted non-sensitive request data")
	}
}

func assertNoDiagnosticSecrets(t *testing.T, stderr string) {
	t.Helper()
	for _, secret := range []string{"api-private", "request-private", "response-private", "abc123", "secret_test_abcdefghijklmnop"} {
		if strings.Contains(stderr, secret) {
			t.Errorf("detailed diagnostics exposed %q", secret)
		}
	}
}

func decodeDiagnostic(t *testing.T, text string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var record map[string]any
	if err := decoder.Decode(&record); err != nil {
		t.Fatalf("invalid JSON diagnostic: %v\n%s", err, text)
	}
	return record
}

func TestDiagnosticsIncludeEveryPage(t *testing.T) {
	resetGlobals(t)
	t.Setenv("TWOCTL_SKIP_UPGRADE_CHECK", "1")
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page++
		w.Header().Set("X-Trace-Id", fmt.Sprintf("page-%d", page))
		if r.URL.Query().Get("page_cursor") == "" {
			fmt.Fprint(w, `{"items":[{"id":1}],"next_page_cursor":"second"}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"id":2}]}`)
	}))
	defer srv.Close()
	stdout, stderr, err := runRoot(t, "--url", srv.URL, "--api-key", "test-key", "billing-statement", "get", "--all", "-v")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) != 2 || !json.Valid([]byte(stdout)) {
		t.Fatalf("expected two diagnostics and JSON stdout: stderr=%s stdout=%s", stderr, stdout)
	}
	for i, line := range lines {
		if decodeDiagnostic(t, line)["request_id"] != fmt.Sprintf("page-%d", i+1) {
			t.Fatalf("wrong page request ID: %s", line)
		}
	}
}

func TestDiagnosticsIncludeHTTPFailures(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprintf("paginated=%t", all), func(t *testing.T) {
			resetGlobals(t)
			t.Setenv("TWOCTL_SKIP_UPGRADE_CHECK", "1")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-Id", "failed-request")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"error":"denied"}`)
			}))
			defer srv.Close()
			args := []string{"--url", srv.URL, "--api-key", "test-key", "billing-statement", "get", "-v"}
			if all {
				args = append(args, "--all")
			}
			_, stderr, err := runRoot(t, args...)
			if err == nil {
				t.Fatal("expected API failure")
			}
			record := decodeDiagnostic(t, stderr)
			if record["request_id"] != "failed-request" || record["status"] != json.Number("403") {
				t.Fatalf("HTTP failure omitted diagnostics: %v", record)
			}
		})
	}
}

type diagnosticFailureTransport struct{}

func (diagnosticFailureTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("transport unavailable")
}

func TestDiagnosticsIncludeTransportErrors(t *testing.T) {
	resetGlobals(t)
	flagVerbose = 2
	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)
	req, _ := http.NewRequest(http.MethodGet, "https://example.test?api_key=api-private", nil)
	err := executeRequest(cmd, &http.Client{Transport: diagnosticFailureTransport{}}, req)
	if err == nil || !strings.Contains(stderr.String(), "transport unavailable") {
		t.Fatalf("lost transport failure: %v / %s", err, stderr.String())
	}
	if strings.Contains(stderr.String(), "api-private") || req.URL.Query().Get("api_key") != "api-private" {
		t.Fatal("diagnostics leaked or modified a sensitive query parameter")
	}
}

func TestDiagnosticBodyLimitsAndUnavailableReplay(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.test", strings.NewReader(strings.Repeat("x", maxResponseBytes+1)))
	if got := diagnosticRequestBody(req); got != "[body exceeds diagnostic limit]" {
		t.Fatalf("unbounded request diagnostic: %v", got)
	}
	req.GetBody = nil
	if got := diagnosticRequestBody(req); got != "[body unavailable]" {
		t.Fatalf("consumed an unreplayable body: %v", got)
	}
	req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("cannot replay") }
	if got := diagnosticRequestBody(req); got != "[body unavailable]" {
		t.Fatalf("replay error not handled: %v", got)
	}
	if got := diagnosticBody([]byte("plain secret_test_abcdefghijklmnop")); got != "plain [REDACTED]" {
		t.Fatalf("plain body secret leaked: %v", got)
	}
	raw := []byte(`[{"Authorization":"private","nested":["secret_test_abcdefghijklmnop"]}]`)
	redacted, _ := json.Marshal(diagnosticBody(raw))
	if strings.Contains(string(redacted), "private") || strings.Contains(string(redacted), "secret_test_") {
		t.Fatalf("nested body secrets leaked: %s", redacted)
	}
}

func TestDiagnosticsRedactKeyShapedValuesUnderOrdinaryNames(t *testing.T) {
	const key = "secret_test_abcdefghijklmnop"
	req, _ := http.NewRequest(http.MethodGet, "https://example.test?note="+key, nil)
	req.Header.Set("X-Custom", key)
	if strings.Contains(diagnosticURL(req), key) {
		t.Fatal("API-key-shaped query value leaked")
	}
	if diagnosticHeaders(req.Header)["X-Custom"] != "[REDACTED]" {
		t.Fatal("API-key-shaped header value leaked")
	}
	if req.URL.Query().Get("note") != key || req.Header.Get("X-Custom") != key {
		t.Fatal("redaction modified the actual request")
	}
}

func TestDiagnosticBodyRedactsURLCredentials(t *testing.T) {
	for _, value := range []string{
		"https://checkout.two.inc/payment/initiation?token=abc123&keep=yes",
		"https://example.test/?ToKeN=abc123&token=second-secret",
		"https://example.test/?%74oken=abc123",
		"/payment/initiation?token=abc123",
		"?token=abc123",
		"https://user:abc123@example.test/path",
		"https://example.test/#token=abc123",
		"https://example.test/?token=abc123&bad=%zz",
		"https://example.test/%zz?token=abc123",
	} {
		t.Run(value, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"payment_link": value, "nested": []any{value}})
			redacted, err := json.Marshal(diagnosticBody(raw))
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"abc123", "second-secret"} {
				if strings.Contains(string(redacted), secret) {
					t.Fatalf("URL credential leaked: %s", redacted)
				}
			}
			if strings.Contains(value, "keep=yes") && !strings.Contains(string(redacted), "keep=yes") {
				t.Fatal("redaction removed non-sensitive query data")
			}
			if got := diagnosticBody([]byte(value)); strings.Contains(fmt.Sprint(got), "abc123") {
				t.Fatalf("plain URL credential leaked: %v", got)
			}
		})
	}
	if got := redactDiagnosticString("ordinary text"); got != "ordinary text" {
		t.Fatalf("ordinary text changed: %q", got)
	}
}
