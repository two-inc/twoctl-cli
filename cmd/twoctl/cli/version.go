package cli

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

func init() {
	version := &cobra.Command{
		Use:   "version",
		Short: "Print twoctl version, Go runtime, and platform",
		Args:  cobra.NoArgs,
		// Local version information must not invoke the inherited upgrade
		// check, especially when a script requests machine-readable output.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE:              runVersion,
	}
	version.Flags().Bool("json", false, "print version information as JSON")
	register(version)
}

type versionInfo struct {
	Version   string `json:"version"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

func versionText(version string) string {
	return fmt.Sprintf("twoctl version %s\ngo %s\nplatform %s/%s\n",
		version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func runVersion(cmd *cobra.Command, _ []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")
	if asJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(versionInfo{
			Version: rootCmd.Version, GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		})
	}
	_, err := fmt.Fprint(cmd.OutOrStdout(), versionText(rootCmd.Version))
	return err
}
