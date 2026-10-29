// Package config holds twoctl's per-environment credentials and base URLs.
//
// Each named environment is a "context" with its own base URL and its own
// API key. Contexts live in ~/.config/twoctl/config.yaml; API keys live in
// the OS keychain under service=twoctl, account=ctx:<name>.
//
// One context is "current" at any time. Commands resolve their key + URL in
// this order:
//
//  1. --api-key flag (key) and --url / --env flags (URL)
//  2. TWO_API_KEY env var
//  3. current context's keychain entry and base URL
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/zalando/go-keyring"
	"gopkg.in/yaml.v3"
)

const (
	keyringService = "twoctl"
	envVar         = "TWO_API_KEY"
)

// Built-in environments. Any name not listed here is mapped to
// https://api.<name>.two.inc when used as a context base URL fallback.
var builtins = map[string]string{
	"prod":    "https://api.two.inc",
	"sandbox": "https://api.sandbox.two.inc",
	"staging": "https://api.staging.two.inc",
	"cyber":   "https://api.cyber.two.inc",
	"perf":    "https://api.perftest.two.inc",
	"release": "https://api.release.two.inc",
}

// Context holds the per-environment settings for one named context.
type Context struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base-url"`
}

// File is the on-disk shape of ~/.config/twoctl/config.yaml.
type File struct {
	CurrentContext string             `yaml:"current-context"`
	Contexts       map[string]Context `yaml:"contexts"`
}

// Resolved captures the materialised configuration for a single invocation.
type Resolved struct {
	APIKey      string
	BaseURL     string
	ContextName string
	Source      string // human-readable provenance, used by `whoami`
}

// ErrNoAPIKey is returned when no key can be located for the active context.
var ErrNoAPIKey = errors.New("no API key found - set TWO_API_KEY, pass --api-key, or run `twoctl auth login`")

// ErrNoContext is returned when no context is configured and no fallback
// flags (--url / --env) were supplied.
var ErrNoContext = errors.New("no context configured - run `twoctl config set-context <name> --url <url>` or pass --env / --url")

// Resolve picks a context (optionally overridden by --env / --url) and pairs
// it with an API key (optionally overridden by --api-key or TWO_API_KEY).
//
// envFlag may be a known context name, a built-in alias (sandbox/prod/...),
// or any user-defined name; rawURL is a direct URL override.
func Resolve(flagKey, envFlag, rawURL string) (*Resolved, error) {
	cfg, err := LoadFile()
	if err != nil {
		return nil, err
	}
	ctxName, baseURL, err := resolveContext(cfg, envFlag, rawURL)
	if err != nil {
		return nil, err
	}
	key, source, err := resolveKey(flagKey, ctxName)
	if err != nil {
		return nil, err
	}
	return &Resolved{
		APIKey: key, BaseURL: baseURL, ContextName: ctxName, Source: source,
	}, nil
}

func resolveContext(cfg *File, envFlag, rawURL string) (string, string, error) {
	if rawURL != "" {
		clean, err := safeURL(rawURL)
		if err != nil {
			return "", "", err
		}
		// --url + --context (or --env) is a credential-leak shape:
		// the API key from <context>'s keychain would be sent to the
		// raw URL. Drop the context name so the key resolution falls
		// through to --api-key / TWO_API_KEY only.
		return "", clean, nil
	}
	if envFlag != "" {
		if ctx, ok := cfg.Contexts[envFlag]; ok {
			return envFlag, ctx.BaseURL, nil
		}
		return envFlag, inferURL(envFlag), nil
	}
	if cfg.CurrentContext != "" {
		if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok {
			return cfg.CurrentContext, ctx.BaseURL, nil
		}
	}
	return "", "", ErrNoContext
}

func resolveKey(flagKey, ctxName string) (string, string, error) {
	if flagKey != "" {
		return flagKey, "--api-key flag", nil
	}
	if v := os.Getenv(envVar); v != "" {
		return v, "$" + envVar, nil
	}
	if ctxName != "" {
		if k, err := keyring.Get(keyringService, keyringAccount(ctxName)); err == nil && k != "" {
			return k, fmt.Sprintf("keychain (context %s)", ctxName), nil
		}
	}
	return "", "", ErrNoAPIKey
}

// inferURL maps a context name to a URL using the built-in table or the
// `api.<name>.two.inc` convention. Used when the user passes --env <name>
// but hasn't registered <name> as a context.
func inferURL(name string) string {
	if u, ok := builtins[name]; ok {
		return u
	}
	return fmt.Sprintf("https://api.%s.two.inc", name)
}

// normaliseURL canonicalises a base URL or returns an error if it would be
// unsafe to send credentials to. Refuses plaintext http:// (except localhost
// for dev), rejects query / fragment / userinfo (which the path-templating
// step would mangle), and demands a parseable scheme + host.
func normaliseURL(u string) string {
	out, _ := safeURL(u)
	return out
}

func safeURL(u string) (string, error) {
	u = strings.TrimRight(u, "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return u, fmt.Errorf("invalid url %q: %w", u, err)
	}
	if parsed.Host == "" {
		return u, fmt.Errorf("url %q has no host", u)
	}
	if parsed.Scheme == "http" && !isLocalHost(parsed.Hostname()) {
		return u, fmt.Errorf("refusing plaintext url %q: API key would be sent unencrypted (use https://, or localhost for dev)", u)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return u, fmt.Errorf("url %q must not include query, fragment, or userinfo", u)
	}
	return u, nil
}

func isLocalHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0"
}

func keyringAccount(ctxName string) string { return "ctx:" + ctxName }

// --- file I/O ---

// configDir returns TWOCTL_CONFIG_HOME when set, otherwise the XDG-style
// $XDG_CONFIG_HOME/twoctl or ~/.config/twoctl directory. The override names
// the twoctl directory itself, not its parent.
func configDir() (string, error) {
	if dir := os.Getenv("TWOCTL_CONFIG_HOME"); dir != "" {
		return dir, nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "twoctl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "twoctl"), nil
}

func filePath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// LoadFile reads the config file. A missing file is not an error; a fresh
// File is returned instead.
func LoadFile() (*File, error) {
	p, err := filePath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &File{Contexts: map[string]Context{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", p, err)
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	if f.Contexts == nil {
		f.Contexts = map[string]Context{}
	}
	return &f, nil
}

// SaveFile atomically writes the config file. The config directory is
// chmod'd to 0o700 and the file itself to 0o600 so other users on shared
// hosts can't read context URLs (or any future cached state).
//
// Callers that mutate config (SetContext / UseContext / DeleteContext)
// wrap the load-modify-write cycle in withLock(); SaveFile itself is the
// final atomic step (CreateTemp + Rename).
func SaveFile(f *File) error {
	p, err := filePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "config-*.yaml")
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	enc := yaml.NewEncoder(tmp)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// --- context CRUD ---

// contextNameRe constrains context names so they can't traverse paths, smuggle
// shell metacharacters, or collide with other apps' keychain entries on
// libsecret-style backends that key on attribute strings.
var contextNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// SetContext inserts or replaces a context. If apiKey is non-empty it is
// stored in the keychain under that context's account. The full read-
// modify-write cycle holds an exclusive flock on config.yaml.lock so two
// concurrent twoctl runs don't lose each other's updates.
func SetContext(name, baseURL, apiKey string) error {
	if !contextNameRe.MatchString(name) {
		return fmt.Errorf("invalid context name %q: must match %s", name, contextNameRe)
	}
	p, err := filePath()
	if err != nil {
		return err
	}
	if err := withLock(p, func() error {
		cfg, err := LoadFile()
		if err != nil {
			return err
		}
		if baseURL == "" {
			baseURL = inferURL(name)
		}
		clean, err := safeURL(baseURL)
		if err != nil {
			return err
		}
		cfg.Contexts[name] = Context{Name: name, BaseURL: clean}
		if cfg.CurrentContext == "" {
			cfg.CurrentContext = name
		}
		return SaveFile(cfg)
	}); err != nil {
		return err
	}
	if apiKey != "" {
		return StoreKey(name, apiKey)
	}
	return nil
}

// UseContext switches the current context. The context must already exist.
// Held under config.yaml.lock for the read-modify-write window.
func UseContext(name string) error {
	p, err := filePath()
	if err != nil {
		return err
	}
	return withLock(p, func() error {
		cfg, err := LoadFile()
		if err != nil {
			return err
		}
		if _, ok := cfg.Contexts[name]; !ok {
			return fmt.Errorf("context %q does not exist (see `twoctl config get-contexts`)", name)
		}
		cfg.CurrentContext = name
		return SaveFile(cfg)
	})
}

// DeleteContext removes a context and its keychain entry.
func DeleteContext(name string) error {
	p, err := filePath()
	if err != nil {
		return err
	}
	if err := withLock(p, func() error {
		cfg, err := LoadFile()
		if err != nil {
			return err
		}
		if _, ok := cfg.Contexts[name]; !ok {
			return fmt.Errorf("context %q does not exist", name)
		}
		delete(cfg.Contexts, name)
		if cfg.CurrentContext == name {
			cfg.CurrentContext = ""
		}
		return SaveFile(cfg)
	}); err != nil {
		return err
	}
	return DeleteKey(name)
}

// ListContexts returns contexts sorted by name. The active context's name is
// also returned for marking in UI output.
func ListContexts() ([]Context, string, error) {
	cfg, err := LoadFile()
	if err != nil {
		return nil, "", err
	}
	names := make([]string, 0, len(cfg.Contexts))
	for n := range cfg.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Context, 0, len(names))
	for _, n := range names {
		out = append(out, cfg.Contexts[n])
	}
	return out, cfg.CurrentContext, nil
}

// --- key storage ---

// StoreKey writes apiKey to the keychain for the given context.
func StoreKey(ctxName, apiKey string) error {
	return wrapKeyringErr(keyring.Set(keyringService, keyringAccount(ctxName), apiKey))
}

// wrapKeyringErr turns the opaque go-keyring failure that happens on headless
// Linux (no libsecret / no DBus session) into something actionable. On macOS
// and Windows it surfaces the underlying error verbatim.
func wrapKeyringErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "secret-service") ||
		strings.Contains(msg, "org.freedesktop.secrets") ||
		strings.Contains(msg, "DBUS_SESSION_BUS_ADDRESS") {
		return fmt.Errorf("no OS keychain backend available - twoctl needs libsecret + a running secret-service (gnome-keyring or KeePassXC). "+
			"On headless servers, set TWO_API_KEY in the environment instead. (cause: %w)", err)
	}
	return err
}

// DeleteKey removes the keychain entry for the given context. A missing
// entry is not an error.
func DeleteKey(ctxName string) error {
	err := keyring.Delete(keyringService, keyringAccount(ctxName))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// HasStoredKey reports whether the keychain currently holds a key for the
// given context.
func HasStoredKey(ctxName string) bool {
	_, err := keyring.Get(keyringService, keyringAccount(ctxName))
	return err == nil
}
