package config

import (
	"os"
	"testing"
)

func TestLoadFromEnv(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		wantFolder  string
		wantDBURL   string
		wantScratch string
		wantDryRun  bool
		wantErr     bool
	}{
		{
			name:        "default values",
			env:         nil,
			wantFolder:  "1B9YW3rbdnOjAjOqOXSJ-pj3tOsWFkOiRbSUyPkEiGfYpqOi0qCADmCwVTiVZEWNQISWkyWeb",
			wantDBURL:   "sqlite:/data/state.db",
			wantScratch: "/scratch",
			wantDryRun:  false,
			wantErr:     false,
		},
		{
			name: "custom environment",
			env: map[string]string{
				"GOOGLE_DRIVE_FOLDER_ID": "custom-folder-123",
				"DBOS_DATABASE_URL":      "sqlite:/tmp/custom.db",
				"SCRATCH_DIR":            "/tmp/scratch",
				"DRY_RUN":                "true",
			},
			wantFolder:  "custom-folder-123",
			wantDBURL:   "sqlite:/tmp/custom.db",
			wantScratch: "/tmp/scratch",
			wantDryRun:  true,
			wantErr:     false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Clear and set env
			os.Clearenv()
			for k, v := range tc.env {
				_ = os.Setenv(k, v)
			}

			cfg, err := LoadFromEnv()
			if (err != nil) != tc.wantErr {
				t.Fatalf("LoadFromEnv() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}

			if cfg.GoogleDriveFolderID != tc.wantFolder {
				t.Errorf("GoogleDriveFolderID = %q, want %q", cfg.GoogleDriveFolderID, tc.wantFolder)
			}
			if cfg.DatabaseURL != tc.wantDBURL {
				t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, tc.wantDBURL)
			}
			if cfg.ScratchDir != tc.wantScratch {
				t.Errorf("ScratchDir = %q, want %q", cfg.ScratchDir, tc.wantScratch)
			}
			if cfg.DryRun != tc.wantDryRun {
				t.Errorf("DryRun = %v, want %v", cfg.DryRun, tc.wantDryRun)
			}
			if tc.name == "default values" && cfg.MetricsAddr != ":9090" {
				t.Errorf("MetricsAddr = %q, want :9090", cfg.MetricsAddr)
			}
		})
	}
}

func TestLoadFromEnv_CredentialsFile(t *testing.T) {
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getting wd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origDir)
	})

	t.Run("explicit GOOGLE_APPLICATION_CREDENTIALS takes precedence", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := os.Chdir(tmpDir); err != nil {
			t.Fatalf("chdir: %v", err)
		}
		// Create default file in tmpDir
		if err := os.WriteFile(DefaultCredentialsFile, []byte(`{}`), 0600); err != nil {
			t.Fatalf("writing default creds file: %v", err)
		}

		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/custom/path/credentials.json")
		cfg, err := LoadFromEnv()
		if err != nil {
			t.Fatalf("LoadFromEnv() unexpected error: %v", err)
		}
		if cfg.GoogleCredentialsFile != "/custom/path/credentials.json" {
			t.Errorf("GoogleCredentialsFile = %q, want %q", cfg.GoogleCredentialsFile, "/custom/path/credentials.json")
		}
	})

	t.Run("auto-detects DefaultCredentialsFile when present", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := os.Chdir(tmpDir); err != nil {
			t.Fatalf("chdir: %v", err)
		}
		if err := os.WriteFile(DefaultCredentialsFile, []byte(`{}`), 0600); err != nil {
			t.Fatalf("writing default creds file: %v", err)
		}

		_ = os.Unsetenv("GOOGLE_APPLICATION_CREDENTIALS")
		cfg, err := LoadFromEnv()
		if err != nil {
			t.Fatalf("LoadFromEnv() unexpected error: %v", err)
		}
		if cfg.GoogleCredentialsFile != DefaultCredentialsFile {
			t.Errorf("GoogleCredentialsFile = %q, want %q", cfg.GoogleCredentialsFile, DefaultCredentialsFile)
		}
	})

	t.Run("empty GoogleCredentialsFile when default file absent", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := os.Chdir(tmpDir); err != nil {
			t.Fatalf("chdir: %v", err)
		}

		_ = os.Unsetenv("GOOGLE_APPLICATION_CREDENTIALS")
		cfg, err := LoadFromEnv()
		if err != nil {
			t.Fatalf("LoadFromEnv() unexpected error: %v", err)
		}
		if cfg.GoogleCredentialsFile != "" {
			t.Errorf("GoogleCredentialsFile = %q, want empty", cfg.GoogleCredentialsFile)
		}
	})
}
