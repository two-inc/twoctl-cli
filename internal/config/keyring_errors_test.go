package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestResolvePreservesKeychainFailure(t *testing.T) {
	for _, message := range []string{
		"could not connect to secret-service",
		"org.freedesktop.secrets returned an error",
		"DBUS_SESSION_BUS_ADDRESS not set",
		"User interaction is not allowed",
	} {
		t.Run(message, func(t *testing.T) {
			withTempHome(t)
			t.Setenv("TWO_API_KEY", "")
			failure := errors.New(message)
			keyring.MockInitWithError(failure)
			t.Cleanup(keyring.MockInit)
			_, err := Resolve("", "sandbox", "")
			if !errors.Is(err, failure) {
				t.Fatalf("lost underlying keychain failure: %v", err)
			}
			if !strings.Contains(err.Error(), "sandbox") {
				t.Fatalf("error omitted the affected context: %v", err)
			}
		})
	}
}

func TestKeychainOperationsExplainMissingBackend(t *testing.T) {
	for name, operation := range map[string]func() error{
		"read":   func() error { _, _, err := resolveKey("", "sandbox"); return err },
		"write":  func() error { return StoreKey("sandbox", "test-key") },
		"delete": func() error { return DeleteKey("sandbox") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TWO_API_KEY", "")
			keyring.MockInitWithError(errors.New("DBUS_SESSION_BUS_ADDRESS not set"))
			t.Cleanup(keyring.MockInit)
			err := operation()
			if err == nil || !strings.Contains(err.Error(), "libsecret") || !strings.Contains(err.Error(), "TWO_API_KEY") {
				t.Fatalf("expected backend guidance, got %v", err)
			}
		})
	}
}

func TestExplicitKeysBypassUnavailableKeychain(t *testing.T) {
	for _, useFlag := range []bool{false, true} {
		withTempHome(t)
		t.Setenv("TWO_API_KEY", "env-key")
		keyring.MockInitWithError(errors.New("DBUS_SESSION_BUS_ADDRESS not set"))
		t.Cleanup(keyring.MockInit)
		flag, want, source := "", "env-key", "$TWO_API_KEY"
		if useFlag {
			flag, want, source = "flag-key", "flag-key", "--api-key flag"
		}
		resolved, err := Resolve(flag, "sandbox", "")
		if err != nil {
			t.Fatal(err)
		}
		if resolved.APIKey != want || resolved.Source != source {
			t.Fatalf("wrong explicit key/source: %+v", resolved)
		}
	}
}

func TestMissingKeyStillReportsNoAPIKey(t *testing.T) {
	withTempHome(t)
	t.Setenv("TWO_API_KEY", "")
	keyring.MockInitWithError(keyring.ErrNotFound)
	t.Cleanup(keyring.MockInit)
	_, err := Resolve("", "sandbox", "")
	if !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("missing key should retain login guidance: %v", err)
	}
	if err := DeleteKey("sandbox"); err != nil {
		t.Fatalf("deleting an absent key should remain a no-op: %v", err)
	}
}
