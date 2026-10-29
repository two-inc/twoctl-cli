package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigHomePrecedence(t *testing.T) {
	for _, source := range []string{"override", "xdg", "home"} {
		t.Run(source, func(t *testing.T) {
			dir := withTempHome(t)
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
			if err := SetContext("sandbox", "", ""); err != nil {
				t.Fatal(err)
			}
			if err := UseContext("sandbox"); err != nil {
				t.Fatal(err)
			}
			assertConfigFile(t, filepath.Join(wantDir, "config.yaml"))
			if _, err := os.Stat(filepath.Join(wantDir, "config.yaml.lock")); err != nil {
				t.Fatalf("lock is not in selected directory: %v", err)
			}
			cfg, err := LoadFile()
			if err != nil || cfg.CurrentContext != "sandbox" {
				t.Fatalf("config did not round-trip: %v, %v", cfg, err)
			}
		})
	}
}

func assertConfigFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestConfigHomeDoesNotModifyDefaultConfig(t *testing.T) {
	dir := withTempHome(t)
	t.Setenv("TWOCTL_CONFIG_HOME", "")
	if err := SetContext("prod", "", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWOCTL_CONFIG_HOME", filepath.Join(dir, "isolated"))
	cfg, err := LoadFile()
	if err != nil || len(cfg.Contexts) != 0 {
		t.Fatalf("override should start empty: %v, %v", cfg, err)
	}
	if err := SetContext("sandbox", "", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWOCTL_CONFIG_HOME", "")
	cfg, err = LoadFile()
	if err != nil || cfg.CurrentContext != "prod" || len(cfg.Contexts) != 1 {
		t.Fatalf("default config changed: %v, %v", cfg, err)
	}
}
