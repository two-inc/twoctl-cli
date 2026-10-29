// Package updater handles self-upgrading the twoctl binary from GitHub
// releases. State (last check time, skipped versions) is persisted to
// ~/.config/twoctl/state.json so the user's "skip this version" choice
// survives across runs.
package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// State is the on-disk preference + check-cache file.
type State struct {
	// NextCheckAt is the earliest UTC time at which we may run the
	// upgrade check again. Stored as an absolute timestamp (rather than
	// "last check + interval") so clock skew, NTP corrections, and
	// suspend/resume don't either suppress or fire the check unexpectedly.
	NextCheckAt        time.Time `json:"next_check_at,omitempty"`
	LatestKnownVersion string    `json:"latest_known_version,omitempty"`
	// CachedRelease holds the full release payload from the last
	// successful GitHub fetch. Caching the assets list means the prompt
	// path can install without a second LatestRelease call (which would
	// silently defeat the throttle).
	CachedRelease   *Release `json:"cached_release,omitempty"`
	SkippedVersions []string `json:"skipped_versions,omitempty"`
	// AutoCheck disables the periodic check entirely when false. Defaults
	// to true on first run; the user can flip it via `twoctl upgrade
	// --disable-autocheck`.
	AutoCheck *bool `json:"auto_check,omitempty"`

	// LastCheckAt is kept for backward-compat with older state files; new
	// writes populate NextCheckAt and leave this blank.
	LastCheckAt time.Time `json:"last_check_at,omitempty"`
}

// IsSkipped reports whether version v has been explicitly skipped by the user.
func (s *State) IsSkipped(v string) bool {
	for _, sv := range s.SkippedVersions {
		if sv == v {
			return true
		}
	}
	return false
}

// AddSkip adds v to the skip list if not already present.
func (s *State) AddSkip(v string) {
	if s.IsSkipped(v) {
		return
	}
	s.SkippedVersions = append(s.SkippedVersions, v)
}

// AutoCheckEnabled returns whether autocheck should run. Defaults to true
// when the field has never been set.
func (s *State) AutoCheckEnabled() bool {
	if s.AutoCheck == nil {
		return true
	}
	return *s.AutoCheck
}

// statePath uses the same directory precedence as context configuration:
// TWOCTL_CONFIG_HOME, $XDG_CONFIG_HOME/twoctl, then ~/.config/twoctl.
func statePath() (string, error) {
	if dir := os.Getenv("TWOCTL_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "state.json"), nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "twoctl", "state.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "twoctl", "state.json"), nil
}

// LoadState reads the state file. A missing file returns an empty State,
// not an error.
func LoadState() (*State, error) {
	p, err := statePath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", p, err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		// A corrupt state file is recoverable - start fresh rather than
		// blocking every command.
		return &State{}, nil
	}
	return &s, nil
}

// SaveState atomically writes the state file.
func SaveState(s *State) error {
	p, err := statePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "state-*.json")
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
