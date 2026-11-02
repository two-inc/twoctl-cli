package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/zalando/go-keyring"
)

// resetCobraFlags walks the entire command tree and resets every flag value
// to its default. Necessary because pflag retains the last Set() value
// across in-process Execute calls, which leaks state between tests.
func resetCobraFlags(cmd *cobra.Command) {
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
	for _, c := range cmd.Commands() {
		resetCobraFlags(c)
	}
}

func resetGlobals(t *testing.T) {
	t.Helper()
	flagAPIKey = ""
	flagEnv = ""
	flagContext = ""
	flagURL = ""
	flagOutput = "json"
	flagMaxRetries = 3
	resetCobraFlags(Root())
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("TWO_API_KEY", "")
	t.Setenv("TWOCTL_CONTEXT", "")
	keyring.MockInit()
}

func runRoot(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	r := Root()
	r.SetOut(&stdout)
	r.SetErr(&stderr)
	r.SetArgs(args)
	err := r.Execute()
	return stdout.String(), stderr.String(), err
}

// TestRootHasTopLevelCommands guards the kubectl-flavored helpers and the
// expected resource subtrees.
func TestRootHasTopLevelCommands(t *testing.T) {
	want := []string{"config", "explain", "version", "api-resources", "auth", "upgrade", "order", "company"}
	got := map[string]bool{}
	for _, c := range Root().Commands() {
		got[c.Name()] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing top-level command: %s", w)
		}
	}
}

func TestResourcesHaveActions(t *testing.T) {
	for _, r := range []string{"order", "company"} {
		var sub *cobra.Command
		for _, c := range Root().Commands() {
			if c.Name() == r {
				sub = c
				break
			}
		}
		if sub == nil {
			t.Errorf("resource %s not registered", r)
			continue
		}
		if len(sub.Commands()) == 0 {
			t.Errorf("resource %s has no actions", r)
		}
	}
}

func TestToKebab(t *testing.T) {
	cases := map[string]string{
		"CreateOrder":           "create-order",
		"create_order":          "create-order",
		"getMerchantSettlement": "get-merchant-settlement",
	}
	for in, want := range cases {
		if got := toKebab(in); got != want {
			t.Errorf("toKebab(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassifyOperation(t *testing.T) {
	cases := []struct {
		opID, method, path, wantRes, wantAction string
	}{
		{"get_order_handler.get", "GET", "/v1/order/{order_id}", "order", "get"},
		{"create_order_handler.post", "POST", "/v1/order", "order", "create"},
		{"cancel_order_handler.post", "POST", "/v1/order/{order_id}/cancel", "order", "cancel"},
		{"refund_order_handler.post", "POST", "/v1/order/{order_id}/refund", "order", "refund"},
		{"edit_order_handler.put", "PUT", "/v1/order/{order_id}", "order", "edit"},
		{"company_search", "GET", "/company", "company", "search"},
		{"fulfill_order_handler.post", "POST", "/v1/order/{order_id}/fulfill", "order", "fulfill"},
	}
	for _, c := range cases {
		op := &openapi3.Operation{OperationID: c.opID}
		r, a := classifyOperation(op, c.method, c.path)
		if r != c.wantRes || a != c.wantAction {
			t.Errorf("classify(%s, %s, %s) = (%s, %s), want (%s, %s)", c.opID, c.method, c.path, r, a, c.wantRes, c.wantAction)
		}
	}
}

func TestParamDescriptionRequired(t *testing.T) {
	p := &openapi3.Parameter{In: "query", Required: true, Description: "the thing"}
	if !strings.Contains(paramDescription(p), "required") {
		t.Error("missing required marker")
	}
}

func TestReadFlagValue(t *testing.T) {
	s := "abc"
	var i int64 = 42
	b := true
	bn := &opBindings{
		stringFlags: map[string]*string{"name": &s, "empty": new(string)},
		intFlags:    map[string]*int64{"count": &i},
		boolFlags:   map[string]*bool{"flag": &b},
	}
	if v, ok := readFlagValue("name", bn); !ok || v != "abc" {
		t.Errorf("string read: %q ok=%v", v, ok)
	}
	if v, ok := readFlagValue("count", bn); !ok || v != "42" {
		t.Errorf("int read: %q ok=%v", v, ok)
	}
	if v, ok := readFlagValue("flag", bn); !ok || v != "true" {
		t.Errorf("bool read: %q ok=%v", v, ok)
	}
	if _, ok := readFlagValue("empty", bn); ok {
		t.Error("empty string should report not present")
	}
}

func TestDescribeOnResource(t *testing.T) {
	resetGlobals(t)
	stdout, _, err := runRoot(t, "order", "get", "--describe")
	if err != nil {
		t.Fatal(err)
	}
	var doc describeDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if doc.Method != "GET" || !strings.Contains(doc.Path, "order") {
		t.Errorf("unexpected describe: %+v", doc)
	}
}

func TestDryRunOnVerbTree(t *testing.T) {
	resetGlobals(t)
	stdout, _, err := runRoot(t, "--url", "http://localhost:9999", "--api-key", "secret_test_x",
		"order", "get", "--order-id", "abc-123", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	var dump map[string]any
	if err := json.Unmarshal([]byte(stdout), &dump); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if dump["method"] != "GET" {
		t.Errorf("method = %v", dump["method"])
	}
	if !strings.Contains(dump["url"].(string), "abc-123") {
		t.Errorf("url missing templated id: %v", dump["url"])
	}
}

func TestRunOperationAgainstMockServer(t *testing.T) {
	resetGlobals(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "secret_test_mock" {
			t.Errorf("X-Api-Key = %q", r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"order_id":"abc-123","state":"VERIFIED"}`))
	}))
	defer srv.Close()

	stdout, _, err := runRoot(t, "--url", srv.URL, "--api-key", "secret_test_mock",
		"order", "get", "--order-id", "abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "VERIFIED") {
		t.Errorf("missing response body:\n%s", stdout)
	}
}

func TestRunOperationHTTPError(t *testing.T) {
	resetGlobals(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"order not found"}`))
	}))
	defer srv.Close()

	_, _, err := runRoot(t, "--url", srv.URL, "--api-key", "secret_test_x",
		"order", "get", "--order-id", "missing")
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("expected *apiError, got %T: %v", err, err)
	}
	if ae.exit != ExitNotFound {
		t.Errorf("exit = %d, want %d", ae.exit, ExitNotFound)
	}
}

func TestFirstLine(t *testing.T) {
	if firstLine("", "  ", "first\nsecond") != "first" {
		t.Error()
	}
	if firstLine("", "") != "" {
		t.Error()
	}
}
