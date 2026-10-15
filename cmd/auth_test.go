package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

//nolint:gosec // test cases contain mock credential JSON fixtures
func TestLoadClientCredentials(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	tests := []struct {
		name        string
		content     string
		wantID      string
		wantSecret  string
		expectError bool
	}{
		// #nosec G101 -- test mock credential structure
		{
			name: "flat client credentials",
			content: `{
				"client_id": "flat-client-id",
				"client_secret": "flat-client-secret"
			}`,
			wantID:     "flat-client-id",
			wantSecret: "flat-client-secret",
		},
		// #nosec G101 -- test mock credential structure
		{
			name: "authorized_user format with refresh_token",
			content: `{
				"type": "authorized_user",
				"client_id": "auth-client-id",
				"client_secret": "auth-client-secret",
				"refresh_token": "some-token"
			}`,
			wantID:     "auth-client-id",
			wantSecret: "auth-client-secret",
		},
		{
			name: "google cloud installed desktop app credentials",
			content: `{
				"installed": {
					"client_id": "installed-client-id",
					"client_secret": "installed-client-secret"
				}
			}`,
			wantID:     "installed-client-id",
			wantSecret: "installed-client-secret",
		},
		{
			name: "google cloud web app credentials",
			content: `{
				"web": {
					"client_id": "web-client-id",
					"client_secret": "web-client-secret"
				}
			}`,
			wantID:     "web-client-id",
			wantSecret: "web-client-secret",
		},
		{
			name:        "invalid json format",
			content:     `{not-valid-json`,
			expectError: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			filePath := filepath.Join(tmpDir, tc.name+".json")
			if err := os.WriteFile(filePath, []byte(tc.content), 0600); err != nil {
				t.Fatalf("writing test file: %v", err)
			}

			id, secret, err := loadClientCredentials(filePath)
			if (err != nil) != tc.expectError {
				t.Fatalf("loadClientCredentials() error = %v, expectError %v", err, tc.expectError)
			}
			if tc.expectError {
				return
			}
			if id != tc.wantID {
				t.Errorf("clientID = %q, want %q", id, tc.wantID)
			}
			if secret != tc.wantSecret {
				t.Errorf("clientSecret = %q, want %q", secret, tc.wantSecret)
			}
		})
	}

	t.Run("nonexistent file", func(t *testing.T) {
		t.Parallel()
		_, _, err := loadClientCredentials(filepath.Join(tmpDir, "does-not-exist.json"))
		if err == nil {
			t.Errorf("expected error for nonexistent file, got nil")
		}
	})
}
