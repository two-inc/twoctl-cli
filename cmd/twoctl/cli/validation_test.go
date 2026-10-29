package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func payloadTestOperation(t *testing.T, schema string) *openapi3.Operation {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(`{"openapi":"3.0.3","info":{"title":"test","version":"1"},"paths":{"/test":{"post":{"requestBody":{"content":{"application/json":{"schema":` + schema + `}}},"responses":{"200":{"description":"ok"}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return doc.Paths.Value("/test").Post
}

const payloadTestSchema = `{"type":"object","required":["amount","currency"],"additionalProperties":false,"properties":{"amount":{"type":"integer","minimum":1},"currency":{"type":"string","enum":["GBP"]},"lines":{"type":"array","items":{"type":"object","required":["name"],"additionalProperties":false,"properties":{"name":{"type":"string"}}}}}`

func TestPayloadValidation(t *testing.T) {
	op := payloadTestOperation(t, payloadTestSchema)
	for _, tc := range []struct {
		name, body, warning string
		invalid             bool
	}{
		{"valid", `{"amount":1,"currency":"GBP"}`, "", false},
		{"unknown", `{"amount":1,"currency":"GBP","extra":"secret"}`, "/extra", false},
		{"nested unknown", `{"amount":1,"currency":"GBP","lines":[{"name":"ok","extra":"secret"}]}`, "/lines/0/extra", false},
		{"missing", `{"ammount":1,"currency":"GBP"}`, "", true},
		{"enum", `{"amount":1,"currency":"secret"}`, "", true},
		{"type", `{"amount":"secret","currency":"GBP"}`, "", true},
		{"minimum", `{"amount":0,"currency":"GBP"}`, "", true},
		{"nested required", `{"amount":1,"currency":"GBP","lines":[{}]}`, "", true},
		{"malformed", `{"secret":`, "", true},
		{"multiple values", `{} {}`, "", true},
		{"null", `null`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var warnings bytes.Buffer
			err := validatePayload(&warnings, op, []byte(tc.body))
			if (err != nil) != tc.invalid {
				t.Fatalf("error = %v", err)
			}
			if err != nil {
				ae, ok := err.(*apiError)
				if !ok || ae.exit != ExitUsage || ae.code != "invalid_request_body" {
					t.Fatalf("unexpected error: %v", err)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatal("payload leaked in error")
				}
			}
			if tc.warning != "" && !strings.Contains(warnings.String(), tc.warning) {
				t.Fatalf("missing warning: %s", warnings.String())
			}
			if tc.warning == "" && warnings.Len() != 0 {
				t.Fatalf("unexpected warning: %s", warnings.String())
			}
			if strings.Contains(warnings.String(), "secret") {
				t.Fatal("payload value leaked in warning")
			}
		})
	}
	schema := op.RequestBody.Value.Content.Get("application/json").Schema.Value
	if schema.AdditionalProperties.Has == nil || *schema.AdditionalProperties.Has {
		t.Fatal("shared schema was changed")
	}
}

func TestPayloadCompositionsAndMaps(t *testing.T) {
	for _, tc := range []struct {
		name, schema, body string
		warn, invalid      bool
	}{
		{"allOf", `{"allOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"integer"}}}]}`, `{"a":"x","b":1}`, false, false},
		{"oneOf", `{"oneOf":[{"type":"object","required":["kind"],"properties":{"kind":{"enum":["a"]}}},{"type":"object","required":["kind"],"properties":{"kind":{"enum":["b"]}}}]}`, `{"kind":"b","extra":1}`, true, false},
		{"anyOf", `{"anyOf":[{"type":"string"},{"type":"object","properties":{"a":{"type":"string"}}}]}`, `{"a":"x","extra":1}`, true, false},
		{"open map", `{"type":"object","additionalProperties":true,"properties":{"a":{"type":"string"}}}`, `{"extra":1}`, false, false},
		{"typed map", `{"type":"object","additionalProperties":{"type":"integer"}}`, `{"a":1}`, false, false},
		{"invalid map", `{"type":"object","additionalProperties":{"type":"integer"}}`, `{"a":"x"}`, false, true},
		{"free form", `{"type":"object"}`, `{"extra":1}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var warnings bytes.Buffer
			err := validatePayload(&warnings, payloadTestOperation(t, tc.schema), []byte(tc.body))
			if (err != nil) != tc.invalid || (warnings.Len() != 0) != tc.warn {
				t.Fatalf("error=%v warnings=%s", err, warnings.String())
			}
		})
	}
}

func TestPayloadValidationBeforeHTTP(t *testing.T) {
	for _, source := range []string{"data", "file", "stdin"} {
		for _, invalid := range []bool{false, true} {
			t.Run(source+map[bool]string{true: "-invalid", false: "-valid"}[invalid], func(t *testing.T) {
				resetGlobals(t)
				body := `{"amount":1,"currency":"GBP","extra":"kept"}`
				if invalid {
					body = `{"amount":1,"currency":"wrong"}`
				}
				calls := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					raw, _ := io.ReadAll(r.Body)
					if string(raw) != body {
						t.Errorf("body changed: %s", raw)
					}
					_, _ = w.Write([]byte(`{"ok":true}`))
				}))
				defer srv.Close()
				flagURL, flagAPIKey = srv.URL, "secret_test_mock"
				cmd := buildOperationCommand("test", "/test", "POST", payloadTestOperation(t, payloadTestSchema))
				args := []string{"--data", body}
				if source != "data" {
					file := filepath.Join(t.TempDir(), "payload.json")
					if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
						t.Fatal(err)
					}
					args = []string{"--file", file}
					if source == "stdin" {
						f, err := os.Open(file)
						if err != nil {
							t.Fatal(err)
						}
						defer f.Close()
						old := os.Stdin
						os.Stdin = f
						defer func() { os.Stdin = old }()
						args = []string{"--file", "-"}
					}
				}
				var stdout, stderr bytes.Buffer
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				cmd.SetArgs(args)
				err := cmd.Execute()
				if invalid {
					if err == nil || calls != 0 || stdout.Len() != 0 {
						t.Fatalf("invalid payload sent: calls=%d err=%v output=%s", calls, err, stdout.String())
					}
				} else if err != nil || calls != 1 || !strings.Contains(stderr.String(), "/extra") || strings.Contains(stdout.String(), "warning") {
					t.Fatalf("calls=%d err=%v stdout=%s stderr=%s", calls, err, stdout.String(), stderr.String())
				}
			})
		}
	}
}

func TestPayloadValidationPagination(t *testing.T) {
	resetGlobals(t)
	op := payloadTestOperation(t, payloadTestSchema)
	op.Parameters = openapi3.Parameters{{Value: &openapi3.Parameter{Name: "page_cursor", In: "query", Schema: &openapi3.SchemaRef{Value: openapi3.NewStringSchema()}}}}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	flagURL, flagAPIKey = srv.URL, "secret_test_mock"
	cmd := buildOperationCommand("test", "/test", "POST", op)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--all", "--data", `{"amount":1}`})
	if err := cmd.Execute(); err == nil || calls != 0 {
		t.Fatalf("invalid paginated body sent: calls=%d err=%v", calls, err)
	}
}

func TestPermissiveSchemaCycle(t *testing.T) {
	s := openapi3.NewObjectSchema()
	s.Properties = openapi3.Schemas{"child": {Value: s}}
	copy := permissiveSchema(s, map[*openapi3.Schema]*openapi3.Schema{})
	if copy == s || copy.Properties["child"].Value != copy {
		t.Fatal("schema cycle was not preserved in private copy")
	}
	if err := validatePayload(io.Discard, &openapi3.Operation{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := validatePayload(io.Discard, &openapi3.Operation{}, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}
