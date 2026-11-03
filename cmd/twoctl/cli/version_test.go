package cli

import (
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestVersionFormsMatch(t *testing.T) {
	resetGlobals(t)
	original := Root().Version
	Root().Version = "v1.2.3-test"
	t.Cleanup(func() { Root().Version = original })
	text, stderr, err := runRoot(t, "version")
	if err != nil || stderr != "" {
		t.Fatalf("version failed: %v, stderr=%q", err, stderr)
	}
	resetCobraFlags(Root())
	flag, stderr, err := runRoot(t, "--version")
	if err != nil || stderr != "" {
		t.Fatalf("--version failed: %v, stderr=%q", err, stderr)
	}
	if flag != text {
		t.Fatalf("version forms disagree:\ncommand: %q\nflag: %q", text, flag)
	}
	for _, want := range []string{"v1.2.3-test", runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH} {
		if !strings.Contains(text, want) {
			t.Errorf("version output omitted %q: %s", want, text)
		}
	}
}

func TestVersionJSONIsOfflineAndMachineReadable(t *testing.T) {
	resetGlobals(t)
	original := Root().PersistentPreRunE
	Root().PersistentPreRunE = func(*cobra.Command, []string) error {
		return errors.New("version must not invoke the inherited upgrade hook")
	}
	t.Cleanup(func() { Root().PersistentPreRunE = original })
	stdout, stderr, err := runRoot(t, "version", "--json")
	if err != nil || stderr != "" {
		t.Fatalf("version --json failed: %v, stderr=%q", err, stderr)
	}
	var info map[string]string
	if err := json.Unmarshal([]byte(stdout), &info); err != nil {
		t.Fatalf("output is not a single JSON object: %v\n%s", err, stdout)
	}
	want := map[string]string{
		"version": Root().Version, "go_version": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
	}
	for key, value := range want {
		if info[key] != value {
			t.Errorf("%s = %q, want %q", key, info[key], value)
		}
	}
}

func TestVersionRejectsUnexpectedArguments(t *testing.T) {
	resetGlobals(t)
	if _, _, err := runRoot(t, "version", "unexpected"); err == nil {
		t.Fatal("version accepted an unexpected positional argument")
	}
}
