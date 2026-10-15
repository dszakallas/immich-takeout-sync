// Package config manages configuration loading and validation from environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Config contains runtime configuration for takeout-sync.
type Config struct {
	// GoogleDriveFolderID is the ID of the folder containing Google Takeout archives.
	GoogleDriveFolderID string

	// DatabaseURL is the DBOS system database connection string (e.g. sqlite:/data/state.db).
	DatabaseURL string

	// ScratchDir is the temporary storage path for downloaded archives.
	ScratchDir string

	// ImmichServerURL is the base URL of the Immich instance (e.g. http://immich-server.apps.svc:2283).
	ImmichServerURL string

	// ImmichAPIKey is the API key used for Immich authentication.
	ImmichAPIKey string

	// GoogleCredentialsFile is an optional path to a credentials JSON file
	// (supports Service Account JSON or OAuth2 token JSON).
	GoogleCredentialsFile string

	// GoogleCredentialsJSON is raw JSON credentials string if passed directly via env var.
	GoogleCredentialsJSON string

	// OAuth2 fallback configuration
	GoogleClientID     string
	GoogleClientSecret string
	GoogleRefreshToken string

	// GoogleAccessToken allows direct OAuth2 bearer token authentication.
	GoogleAccessToken string

	// ImmichGoBinary is the binary name or path for immich-go. Defaults to "immich-go".
	ImmichGoBinary string

	// DeleteFromDrive moves archives to Google Drive trash after successful import if true.
	// Defaults to false (safe mode).
	DeleteFromDrive bool

	// DiskHighWatermark is the disk usage threshold (0.0 to 1.0) above which cached zips are evicted.
	// Defaults to 0.90 (90%).
	DiskHighWatermark float64

	// MaxQueuedDownloads limits background download queue. Defaults to 1.
	MaxQueuedDownloads int

	// MetricsAddr is the address for the Prometheus metrics HTTP server (e.g. ":9090").
	// If empty or "none", the metrics server is disabled. Defaults to ":9090".
	MetricsAddr string

	// DryRun skips actual uploads to Immich and trashing in Google Drive if set to true.
	DryRun bool
}

// AppName is the application identifier used for XDG configuration paths and workflow identification.
const AppName = "immich-takeout-sync"

// DefaultCredentialsFileName is the standard file name for saved OAuth2 credentials.
// #nosec G101 -- default credentials file name, not a secret
const DefaultCredentialsFileName = "creds.json"

// XDGConfigHome returns the base XDG configuration directory ($XDG_CONFIG_HOME or ~/.config).
func XDGConfigHome() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config")
}

// DefaultCredentialsPath returns the standard path to the credentials file:
// xdg_config_home/app_name/creds.json.
func DefaultCredentialsPath() string {
	base := XDGConfigHome()
	if base == "" {
		return DefaultCredentialsFileName
	}
	return filepath.Join(base, AppName, DefaultCredentialsFileName)
}

// LoadFromEnv loads configuration from environment variables with sensible defaults.
func LoadFromEnv() (*Config, error) {
	credsFile := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if credsFile == "" {
		candidates := []string{
			DefaultCredentialsPath(),
			filepath.Join(XDGConfigHome(), "takeout-sync", DefaultCredentialsFileName),
			"google-oauth2-credentials.json",
			"creds.json",
		}
		for _, cand := range candidates {
			if cand == "" {
				continue
			}
			if _, err := os.Stat(cand); err == nil {
				credsFile = cand
				break
			}
		}
	}

	cfg := &Config{
		GoogleDriveFolderID:   getEnv("GOOGLE_DRIVE_FOLDER_ID", "1B9YW3rbdnOjAjOqOXSJ-pj3tOsWFkOiRbSUyPkEiGfYpqOi0qCADmCwVTiVZEWNQISWkyWeb"),
		DatabaseURL:           getEnv("DBOS_DATABASE_URL", "sqlite:/data/state.db"),
		ScratchDir:            getEnv("SCRATCH_DIR", "/scratch"),
		ImmichServerURL:       getEnv("IMMICH_SERVER_URL", "http://immich-server.apps.svc:2283"),
		ImmichAPIKey:          os.Getenv("IMMICH_API_KEY"),
		GoogleCredentialsFile: credsFile,
		GoogleCredentialsJSON: os.Getenv("GOOGLE_CREDENTIALS_JSON"),
		GoogleAccessToken:     os.Getenv("GOOGLE_ACCESS_TOKEN"),
		GoogleClientID:        os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret:    os.Getenv("GOOGLE_CLIENT_SECRET"),
		GoogleRefreshToken:    os.Getenv("GOOGLE_REFRESH_TOKEN"),
		ImmichGoBinary:        getEnv("IMMICH_GO_BIN", "immich-go"),
		DeleteFromDrive:       os.Getenv("DELETE_FROM_DRIVE") == "true" || os.Getenv("GOOGLE_DRIVE_DELETE_AFTER_IMPORT") == "true",
		MetricsAddr:           getEnv("METRICS_ADDR", ":9090"),
		DiskHighWatermark:     0.90,
		MaxQueuedDownloads:    1,
		DryRun:                os.Getenv("DRY_RUN") == "true" || os.Getenv("DRY_RUN") == "1",
	}

	if cfg.GoogleDriveFolderID == "" {
		return nil, fmt.Errorf("GOOGLE_DRIVE_FOLDER_ID is required")
	}

	return cfg, nil
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
