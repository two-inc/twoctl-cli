package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/two-inc/twoctl-cli/internal/config"
	"github.com/two-inc/twoctl-cli/internal/updater"
)

func TestReadBody(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "body.json")
	_ = os.WriteFile(p, []byte(`{"a":1}`), 0o600)

	if b, err := readBody(p, ""); err != nil || string(b) != `{"a":1}` {
		t.Errorf("file: %v %q", err, b)
	}
	if b, err := readBody("", `{"k":"v"}`); err != nil || string(b) != `{"k":"v"}` {
		t.Errorf("data: %v %q", err, b)
	}
	if b, err := readBody("", ""); err != nil || b != nil {
		t.Errorf("nil: %v %q", err, b)
	}
}

func TestCopyHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://x", nil)
	in := http.Header{}
	in.Set("X-Foo", "1")
	in.Add("X-Multi", "a")
	in.Add("X-Multi", "b")
	copyHeaders(req, in)
	if req.Header.Get("X-Foo") != "1" {
		t.Error()
	}
	if vs := req.Header.Values("X-Multi"); len(vs) != 2 {
		t.Errorf("multi-value lost: %v", vs)
	}
}

func TestOriginalParamName(t *testing.T) {
	op := &openapi3.Operation{
		Parameters: openapi3.Parameters{
			&openapi3.ParameterRef{Value: &openapi3.Parameter{Name: "X-Trace-Id", In: "header"}},
		},
	}
	if got := originalParamName(op, "x-trace-id"); got != "X-Trace-Id" {
		t.Errorf("got %q", got)
	}
	if got := originalParamName(op, "missing"); got != "missing" {
		t.Errorf("got %q", got)
	}
}

func TestPickContextOverride(t *testing.T) {
	got, err := pickContext("explicit")
	if err != nil || got != "explicit" {
		t.Errorf("%v %q", err, got)
	}
}

func TestPickContextFromConfig(t *testing.T) {
	resetGlobals(t)
	_ = config.SetContext("sandbox", "", "")
	if got, _ := pickContext(""); got != "sandbox" {
		t.Errorf("got %q", got)
	}
}

func TestPickContextNoCurrent(t *testing.T) {
	resetGlobals(t)
	if _, err := pickContext(""); err == nil {
		t.Error("expected error")
	}
}

func TestRunOperationWithRequestBody(t *testing.T) {
	resetGlobals(t)
	var seen []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		seen = buf[:n]
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		_, _ = w.Write([]byte(`{"id":"new"}`))
	}))
	defer srv.Close()

	flagURL, flagAPIKey = srv.URL, "secret_test_x"
	cmd := buildOperationCommand("test", "/test", "POST", payloadTestOperation(t, `{"type":"object","properties":{"foo":{"type":"string"}}}`))
	var output strings.Builder
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--data", `{"foo":"bar"}`})
	err := cmd.Execute()
	stdout := output.String()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(seen), `"foo":"bar"`) {
		t.Errorf("body not sent: %q", seen)
	}
	if !strings.Contains(stdout, `"id"`) {
		t.Errorf("missing response: %s", stdout)
	}
}

func TestRunOperationEmptyResponse(t *testing.T) {
	resetGlobals(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_, stderr, err := runRoot(t, "--url", srv.URL, "--api-key", "secret_test_x",
		"order", "get", "--order-id", "x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "no body") {
		t.Errorf("expected 'no body' on stderr: %q", stderr)
	}
}

func TestRunOperationYAMLOutput(t *testing.T) {
	resetGlobals(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"k":"v"}`))
	}))
	defer srv.Close()

	stdout, _, err := runRoot(t, "--url", srv.URL, "--api-key", "secret_test_x", "-o", "yaml",
		"order", "get", "--order-id", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "k: v") {
		t.Errorf("yaml missing:\n%s", stdout)
	}
}

func TestAuthLoginInvalidKey(t *testing.T) {
	resetGlobals(t)
	_ = config.SetContext("sandbox", "", "")
	resetCobraFlags(Root())
	if _, _, err := runRoot(t, "auth", "login", "--key", "not-a-secret"); err == nil {
		t.Error("expected error for bogus key")
	}
}

func TestAuthLoginStores(t *testing.T) {
	resetGlobals(t)
	_ = config.SetContext("sandbox", "", "")
	resetCobraFlags(Root())
	if _, _, err := runRoot(t, "auth", "login", "--key", "secret_test_validkey1234567890abcd"); err != nil {
		t.Fatal(err)
	}
	if !config.HasStoredKey("sandbox") {
		t.Error("key not stored")
	}
}

func TestAuthLogoutRemoves(t *testing.T) {
	resetGlobals(t)
	_ = config.SetContext("sandbox", "", "secret_test_xx")
	resetCobraFlags(Root())
	if _, _, err := runRoot(t, "auth", "logout"); err != nil {
		t.Fatal(err)
	}
	if config.HasStoredKey("sandbox") {
		t.Error("key not removed")
	}
}

func TestUpgradeResetSkips(t *testing.T) {
	resetGlobals(t)
	s := &updater.State{SkippedVersions: []string{"v1.0.0"}}
	_ = updater.SaveState(s)
	resetCobraFlags(Root())

	stdout, _, _ := runRoot(t, "upgrade", "--reset-skips", "--check")
	if !strings.Contains(stdout, "cleared skipped") {
		t.Errorf("missing confirmation:\n%s", stdout)
	}
}

func TestUpgradeDisableEnableAutocheck(t *testing.T) {
	resetGlobals(t)
	if _, _, err := runRoot(t, "upgrade", "--disable-autocheck"); err != nil {
		t.Fatal(err)
	}
	s, _ := updater.LoadState()
	if s.AutoCheckEnabled() {
		t.Error("disable failed")
	}

	resetCobraFlags(Root())
	if _, _, err := runRoot(t, "upgrade", "--enable-autocheck"); err != nil {
		t.Fatal(err)
	}
	s, _ = updater.LoadState()
	if !s.AutoCheckEnabled() {
		t.Error("enable failed")
	}
}

func TestDryRunBaseURL(t *testing.T) {
	resetGlobals(t)
	flagURL = "http://x"
	if dryRunBaseURL() != "http://x" {
		t.Error()
	}
	flagURL = ""
	flagEnv = "cyber"
	if dryRunBaseURL() != "https://api.cyber.two.inc" {
		t.Error()
	}
	flagEnv = ""
	if dryRunBaseURL() != "https://api.sandbox.two.inc" {
		t.Error()
	}
}

func TestActiveEnv(t *testing.T) {
	flagEnv = "env-val"
	flagContext = ""
	if activeEnv() != "env-val" {
		t.Error()
	}
	flagContext = "ctx-val"
	if activeEnv() != "ctx-val" {
		t.Error("--context should win over --env")
	}
	flagEnv = ""
	flagContext = ""
}

func TestLooksLikeAPIKey(t *testing.T) {
	cases := map[string]bool{
		"secret_test_abcdef1234567890abcd": true,
		"secret_prod_abcdef1234567890abcd": true,
		"plain":                            false,
		"":                                 false,
	}
	for in, want := range cases {
		if got := looksLikeAPIKey(in); got != want {
			t.Errorf("looksLikeAPIKey(%q) = %v", in, got)
		}
	}
}

func TestRedactKey(t *testing.T) {
	if redactKey("secret_test_aaaaaaaaaaaa1234") != "secret_test_****1234" {
		t.Error("expected env-prefix kept")
	}
	if redactKey("short") != "****" {
		t.Error("short keys fully redact")
	}
}

func TestOrDash(t *testing.T) {
	if orDash("") != "-" {
		t.Error()
	}
	if orDash("x") != "x" {
		t.Error()
	}
}

func TestExitForStatus(t *testing.T) {
	cases := map[int]int{
		http.StatusOK:                  ExitGeneric,
		http.StatusUnauthorized:        ExitAuth,
		http.StatusForbidden:           ExitAuth,
		http.StatusNotFound:            ExitNotFound,
		http.StatusTooManyRequests:     ExitRateLimited,
		http.StatusInternalServerError: ExitServer,
		http.StatusBadRequest:          ExitGeneric,
	}
	for status, want := range cases {
		if got := exitForStatus(status); got != want {
			t.Errorf("exitForStatus(%d) = %d, want %d", status, got, want)
		}
	}
}

func TestRedactHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Api-Key", "secret_test_longenoughkey1234")
	h.Set("Accept", "application/json")
	out := redactHeaders(h)
	if out["X-Api-Key"] == "secret_test_longenoughkey1234" {
		t.Error("not redacted")
	}
	if out["Accept"] != "application/json" {
		t.Error("non-sensitive should pass through")
	}
}

func TestStripPythonLocals(t *testing.T) {
	in := "make_billing_statements_handler.<locals>.get_billing_statements"
	if got := stripPythonLocals(in); got != "get_billing_statements" {
		t.Errorf("got %q", got)
	}
}

func TestDisplayCurrent(t *testing.T) {
	if displayCurrent() == "" {
		t.Error("never empty")
	}
}
