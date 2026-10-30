package cli

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/two-inc/twoctl-cli/internal/config"
)

func init() {
	authCmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage API keys for the active context",
		Long: `Store, remove, and inspect the API key for a context.

By default, auth subcommands operate on the current context (set by
` + "`twoctl config use-context`" + `). Pass --context or set TWOCTL_CONTEXT to target a different one.`,
	}
	authCmd.AddCommand(
		authLoginCmd(),
		authLogoutCmd(),
		authWhoamiCmd(),
	)
	register(authCmd)
}

func authLoginCmd() *cobra.Command {
	var keyFlag, ctxFlag string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store an API key in the OS keychain for a context",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := pickContext(ctxFlag)
			if err != nil {
				return err
			}
			key := strings.TrimSpace(keyFlag)
			if key == "" {
				k, err := promptAPIKey(cmd)
				if err != nil {
					return err
				}
				key = k
			}
			if !looksLikeAPIKey(key) {
				return fmt.Errorf("that does not look like a Two API key (expected prefix secret_test_ or secret_prod_)")
			}
			if err := config.StoreKey(target, key); err != nil {
				return fmt.Errorf("storing key in keychain: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stored API key for context %q\n", target)
			return nil
		},
	}
	cmd.Flags().StringVar(&keyFlag, "key", "", "API key to store (will prompt if omitted)")
	cmd.Flags().StringVar(&ctxFlag, "context", "", "context to store the key under (default: current context)")
	return cmd
}

func authLogoutCmd() *cobra.Command {
	var ctxFlag string
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove the API key for a context",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := pickContext(ctxFlag)
			if err != nil {
				return err
			}
			if err := config.DeleteKey(target); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed API key for context %q\n", target)
			return nil
		},
	}
	cmd.Flags().StringVar(&ctxFlag, "context", "", "context to forget (default: current context)")
	return cmd
}

func authWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the active context, API URL, and key provenance",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := config.Resolve(flagAPIKey, activeEnv(), flagURL)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "context:    %s\nurl:        %s\nkey source: %s\nkey prefix: %s\n",
				orDash(resolved.ContextName), resolved.BaseURL, resolved.Source, redactKey(resolved.APIKey))
			return nil
		},
	}
}

// pickContext returns the explicit override if provided, otherwise the
// invocation context (--context / --env / TWOCTL_CONTEXT), then the saved
// context name. Errors if none is set.
func pickContext(override string) (string, error) {
	if override == "" {
		override = activeEnv()
	}
	if override != "" {
		return override, nil
	}
	cfg, err := config.LoadFile()
	if err != nil {
		return "", err
	}
	if cfg.CurrentContext == "" {
		return "", fmt.Errorf("no current context - pass --context or run `twoctl config set-context <name> --url <url>`")
	}
	return cfg.CurrentContext, nil
}

func promptAPIKey(cmd *cobra.Command) (string, error) {
	fmt.Fprint(cmd.OutOrStdout(), "Two API key: ")
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(cmd.OutOrStdout())
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// apiKeyRe accepts Two API keys: one of four env prefixes followed by at
// least 16 URL-safe characters. The strict regex prevents:
//   - "secret_test_x" (too short) being stored
//   - whitespace, CRLF, or other control characters embedded in the key
//     (CRLF would otherwise be rejected by net/http at write time, but
//     only after the bad value was cached in the keychain)
//   - arbitrary attacker-supplied prefixes being accepted
var apiKeyRe = regexp.MustCompile(`^secret_(test|prod|live|sandbox)_[A-Za-z0-9_\-]{16,256}$`)

func looksLikeAPIKey(s string) bool {
	return apiKeyRe.MatchString(s)
}

// redactKey returns the key with everything between the env prefix and the
// last four characters masked. For inputs that don't match looksLikeAPIKey
// the redaction is conservative: just `****` with no tail leakage.
func redactKey(key string) string {
	if !looksLikeAPIKey(key) {
		return "****"
	}
	tail := key[len(key)-4:]
	idx := strings.Index(key, "_")
	idx2 := strings.Index(key[idx+1:], "_")
	return key[:idx+1+idx2+1] + "****" + tail
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
