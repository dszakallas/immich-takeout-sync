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
