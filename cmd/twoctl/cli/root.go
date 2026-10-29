// Package cli wires the cobra command tree for twoctl. Generated subcommands
// (one tree per API) are registered from init() functions in sibling files.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/two-inc/twoctl-cli/internal/httpx"
)

// Global flags. Populated by cobra during Execute().
var (
	flagAPIKey  string
	flagEnv     string
	flagContext string
	flagURL     string
	flagOutput  string
)

// activeEnv returns the context-selecting flag value, preferring --context
// over --env when both are set (kubectl-style precedence).
func activeEnv() string {
	if flagContext != "" {
		return flagContext
	}
	return flagEnv
}

var rootCmd = &cobra.Command{
	Use:   "twoctl",
	Short: "Command-line interface for the Two merchant APIs",
	Long: `twoctl is a command-line interface for the Two merchant APIs.
The command tree is resource-first: ` + "`twoctl <resource> <action>`" + `.
Every operation across the twelve published Two OpenAPI specs surfaces as
a subcommand, so coverage tracks the API surface 1:1.

Register a context once, then run any command:

    twoctl config set-context sandbox --url https://api.sandbox.two.inc --key secret_test_...
    twoctl order get --order-id abc

--env / --context selects a context for one command without switching (any
registered context, or one of the built-in aliases: prod, sandbox, staging,
cyber, perf, release). For unknown names twoctl infers https://api.<name>.two.inc;
override with --url. --api-key overrides the key stored for that context.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&flagAPIKey, "api-key", "", "API key (overrides keychain + TWO_API_KEY)")
	rootCmd.PersistentFlags().StringVar(&flagEnv, "env", "", "context to use for this command without switching (also accepts sandbox/prod/staging/cyber/perf/release as built-ins)")
	rootCmd.PersistentFlags().StringVar(&flagContext, "context", "", "alias of --env (kubectl-style)")
	rootCmd.PersistentFlags().StringVar(&flagURL, "url", "", "raw API base URL (overrides the context's URL)")
	rootCmd.PersistentFlags().StringVarP(&flagOutput, "output", "o", "auto", "output format: table, json, yaml (auto = table on TTY, json when piped)")
	rootCmd.Version = httpx.Version
	rootCmd.SetVersionTemplate(versionText("{{.Version}}"))
}

// Root returns the configured root cobra.Command.
func Root() *cobra.Command { return rootCmd }

// register adds a subcommand to the root. Used by init() functions in
// sibling files that build their own command subtrees.
func register(c *cobra.Command) { rootCmd.AddCommand(c) }
