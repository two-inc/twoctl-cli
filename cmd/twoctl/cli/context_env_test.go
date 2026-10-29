package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/two-inc/twoctl-cli/internal/config"
)

func TestContextEnvironmentPrecedence(t *testing.T) {
	cases := []struct {
		name  string
		env   string
		flags []string
		want  string
	}{
		{"saved", "", nil, "saved"},
		{"environment", "job", nil, "job"},
		{"env flag", "job", []string{"--env", "explicit"}, "explicit"},
		{"context flag", "job", []string{"--context", "explicit"}, "explicit"},
		{"both flags", "job", []string{"--env", "job", "--context", "explicit"}, "explicit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetGlobals(t)
			t.Setenv("TWOCTL_CONTEXT", tc.env)
			t.Setenv("TWOCTL_SKIP_UPGRADE_CHECK", "1")
			var path, key string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path, key = r.URL.Path, r.Header.Get("X-Api-Key")
				fmt.Fprint(w, `{"items":[]}`)
			}))
			defer srv.Close()
			for _, name := range []string{"saved", "job", "explicit"} {
				if err := config.SetContext(name, srv.URL+"/"+name, name+"-key"); err != nil {
					t.Fatal(err)
				}
			}
			args := append(append([]string{}, tc.flags...), "billing-statement", "get")
			if _, _, err := runRoot(t, args...); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(path, "/"+tc.want+"/") || key != tc.want+"-key" {
				t.Fatalf("selected path=%q key=%q, want context %s", path, key, tc.want)
			}
			assertSavedContext(t, "saved")
		})
	}
}

func assertSavedContext(t *testing.T, want string) {
	t.Helper()
	cfg, err := config.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != want {
		t.Fatalf("saved context changed to %q, want %q", cfg.CurrentContext, want)
	}
}

func TestContextEnvironmentDryRunWithoutKey(t *testing.T) {
	resetGlobals(t)
	t.Setenv("TWOCTL_CONTEXT", "job")
	t.Setenv("TWOCTL_SKIP_UPGRADE_CHECK", "1")
	if err := config.SetContext("job", "https://custom.example.test/api", ""); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runRoot(t, "billing-statement", "get", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		URL     string      `json:"url"`
		Headers http.Header `json:"headers"`
	}
	if err := json.Unmarshal([]byte(stdout), &request); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(request.URL, "https://custom.example.test/api/") {
		t.Fatalf("dry-run ignored the registered context URL: %s", request.URL)
	}
	if request.Headers.Get("X-Api-Key") != "" {
		t.Fatal("dry-run injected a placeholder API key")
	}
}

func TestContextEnvironmentRawURLRequiresExplicitKey(t *testing.T) {
	resetGlobals(t)
	t.Setenv("TWOCTL_CONTEXT", "job")
	t.Setenv("TWOCTL_SKIP_UPGRADE_CHECK", "1")
	if err := config.SetContext("job", "", "context-key"); err != nil {
		t.Fatal(err)
	}
	_, _, err := runRoot(t, "--url", "https://override.example.test", "billing-statement", "get")
	if err != config.ErrNoAPIKey {
		t.Fatalf("raw URL must not use an environment context's key: %v", err)
	}
}

func TestContextEnvironmentAuthCommands(t *testing.T) {
	for _, command := range []string{"login", "logout", "whoami"} {
		t.Run(command, func(t *testing.T) {
			resetGlobals(t)
			t.Setenv("TWOCTL_CONTEXT", "job")
			t.Setenv("TWOCTL_SKIP_UPGRADE_CHECK", "1")
			for _, name := range []string{"saved", "job"} {
				if err := config.SetContext(name, "", "secret_test_abcdefghijklmnop"); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"auth", command}
			if command == "login" {
				args = append(args, "--key", "secret_test_zyxwvutsrqponmlk")
			}
			stdout, _, err := runRoot(t, args...)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stdout, "job") {
				t.Fatalf("auth %s did not target environment context: %s", command, stdout)
			}
			if command == "logout" && config.HasStoredKey("job") {
				t.Fatal("logout did not remove the environment context's key")
			}
			if command == "login" {
				resolved, err := config.Resolve("", "job", "")
				if err != nil || resolved.APIKey != "secret_test_zyxwvutsrqponmlk" {
					t.Fatalf("login did not update the environment context's key: %v", err)
				}
			}
			if !config.HasStoredKey("saved") {
				t.Fatal("auth command affected saved context's key")
			}
			assertSavedContext(t, "saved")
		})
	}
}

func TestAuthExplicitContextOverridesEnvironment(t *testing.T) {
	resetGlobals(t)
	t.Setenv("TWOCTL_CONTEXT", "job")
	got, err := pickContext("explicit")
	if err != nil || got != "explicit" {
		t.Fatalf("explicit context lost precedence: %q, %v", got, err)
	}
}
