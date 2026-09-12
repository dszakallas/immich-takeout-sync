package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestGetBuildInfo(t *testing.T) {
	t.Parallel()

	info := GetBuildInfo()
	if info.Version == "" {
		t.Error("expected non-empty Version")
	}
	if info.GoVersion == "" {
		t.Error("expected non-empty GoVersion")
	}
	if info.OS == "" {
		t.Error("expected non-empty OS")
	}
	if info.Arch == "" {
		t.Error("expected non-empty Arch")
	}
}

func TestVersionCommand(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantPrefix string
	}{
		{
			name:       "default output",
			args:       []string{"version"},
			wantPrefix: "takeout-sync",
		},
		{
			name:       "short output",
			args:       []string{"version", "-s"},
			wantPrefix: Version,
		},
		{
			name:       "json output",
			args:       []string{"version", "--json"},
			wantPrefix: "{\n  \"version\":",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_ = versionCmd.Flags().Set("short", "false")
			_ = versionCmd.Flags().Set("json", "false")

			buf := new(bytes.Buffer)
			rootCmd.SetOut(buf)
			rootCmd.SetErr(buf)
			rootCmd.SetArgs(tc.args)

			err := rootCmd.Execute()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			output := buf.String()
			if !strings.HasPrefix(output, tc.wantPrefix) {
				t.Errorf("expected output to start with %q, got %q", tc.wantPrefix, output)
			}
		})
	}
}
