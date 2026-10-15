package cmd

import (
	"testing"
)

func TestCredentialsFileFlag(t *testing.T) {
	orig := credentialsFile
	t.Cleanup(func() {
		credentialsFile = orig
	})

	flag := rootCmd.PersistentFlags().Lookup("credentials-file")
	if flag == nil {
		t.Fatal("expected --credentials-file persistent flag to be registered on rootCmd")
	}
	if flag.Shorthand != "c" {
		t.Errorf("shorthand = %q, want %q", flag.Shorthand, "c")
	}
}
