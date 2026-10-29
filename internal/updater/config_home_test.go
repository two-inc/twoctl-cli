package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateConfigHomePrecedence(t *testing.T) {
	for _, source := range []string{"override", "xdg", "home"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("XDG_CONFIG_HOME", dir)
			t.Setenv("TWOCTL_CONFIG_HOME", "")
			wantDir := filepath.Join(dir, "twoctl")
			if source == "override" {
				wantDir = filepath.Join(dir, "job config")
				t.Setenv("TWOCTL_CONFIG_HOME", wantDir)
			}
			if source == "home" {
				t.Setenv("XDG_CONFIG_HOME", "")
				wantDir = filepath.Join(dir, ".config", "twoctl")
			}
			if err := SaveState(&State{SkippedVersions: []string{"v1.0.0"}}); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(wantDir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("state permissions = %o, want 600", info.Mode().Perm())
			}
			state, err := LoadState()
			if err != nil || !state.IsSkipped("v1.0.0") {
				t.Fatalf("state did not round-trip: %v, %v", state, err)
			}
		})
	}
}

func TestStateConfigHomeDoesNotModifyDefaultState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("TWOCTL_CONFIG_HOME", "")
	if err := SaveState(&State{SkippedVersions: []string{"v1.0.0"}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWOCTL_CONFIG_HOME", filepath.Join(dir, "isolated"))
	state, err := LoadState()
	if err != nil || len(state.SkippedVersions) != 0 {
		t.Fatalf("override should start empty: %v, %v", state, err)
	}
	if err := SaveState(&State{SkippedVersions: []string{"v2.0.0"}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWOCTL_CONFIG_HOME", "")
	state, err = LoadState()
	if err != nil || !state.IsSkipped("v1.0.0") || state.IsSkipped("v2.0.0") {
		t.Fatalf("default state changed: %v, %v", state, err)
	}
}
