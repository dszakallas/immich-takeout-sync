package cmd

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

var (
	// Version is the build version, set at compile time via -ldflags.
	Version = "dev"
	// Commit is the git commit SHA, set at compile time via -ldflags.
	Commit = "none"
	// Date is the build timestamp, set at compile time via -ldflags.
	Date = "unknown"
)

// BuildInfo represents version and build metadata.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// GetBuildInfo returns the current build information.
func GetBuildInfo() BuildInfo {
	return BuildInfo{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print build and version information",
	RunE: func(cmd *cobra.Command, _ []string) error {
		info := GetBuildInfo()

		short, _ := cmd.Flags().GetBool("short")
		if short {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), info.Version)
			return err
		}

		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(info)
		}

		_, err := fmt.Fprintf(cmd.OutOrStdout(), "takeout-sync %s (commit: %s, built: %s, %s %s/%s)\n",
			info.Version, info.Commit, info.Date, info.GoVersion, info.OS, info.Arch)
		return err
	},
}

func init() {
	versionCmd.Flags().BoolP("short", "s", false, "Print only the version string")
	versionCmd.Flags().Bool("json", false, "Print version info in JSON format")
}
