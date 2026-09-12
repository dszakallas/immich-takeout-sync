// Package drive provides a Google Drive client for discovering, downloading, and soft-deleting Takeout archives.
package drive

import (
	"context"
	// #nosec G501 -- MD5 is provided by Google Drive API for download integrity verification
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dszakallas/immich-takeout-sync/internal/config"
	"github.com/dszakallas/immich-takeout-sync/internal/metrics"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// FileInfo holds metadata about a Takeout archive file in Google Drive.
type FileInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	MD5Checksum string `json:"md5Checksum"`
	LocalPath   string `json:"localPath,omitempty"`
}

// TakeoutBatch represents a single Google Takeout export containing one or more split archive files.
type TakeoutBatch struct {
	ID         string     `json:"id"`
	ExportDate time.Time  `json:"exportDate"`
	Files      []FileInfo `json:"files"`
	TotalBytes int64      `json:"totalBytes"`
}

// Client interacts with the Google Drive API.
type Client struct {
	service *drive.Service
	metrics *metrics.Metrics
}

// SetMetrics attaches a metrics collector to the client.
func (c *Client) SetMetrics(m *metrics.Metrics) {
	c.metrics = m
}

type oauthTokenConfig struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

// NewClient initializes a Google Drive API client supporting Service Account or OAuth2 user tokens.
func NewClient(ctx context.Context, cfg *config.Config) (*Client, error) {
	var opts []option.ClientOption

	rawCreds := []byte(cfg.GoogleCredentialsJSON)
	if len(rawCreds) == 0 && cfg.GoogleCredentialsFile != "" {
		cleanedPath := filepath.Clean(cfg.GoogleCredentialsFile)
		data, err := os.ReadFile(cleanedPath)
		if err != nil {
			return nil, fmt.Errorf("reading google credentials file %s: %w", cleanedPath, err)
		}
		rawCreds = data
	}

	if cfg.GoogleAccessToken != "" {
		slog.Info("Authenticating with Google Drive using direct OAuth2 access token")
		token := &oauth2.Token{AccessToken: cfg.GoogleAccessToken}
		opts = append(opts, option.WithTokenSource(oauth2.StaticTokenSource(token)))
	} else if len(rawCreds) > 0 {
		var peek map[string]any
		if err := json.Unmarshal(rawCreds, &peek); err == nil {
			if credType, ok := peek["type"].(string); ok && credType == "service_account" {
				slog.Info("Authenticating with Google Drive using Service Account")
				opts = append(opts, option.WithAuthCredentialsJSON(option.ServiceAccount, rawCreds))
			} else if tok, ok := peek["access_token"].(string); ok && tok != "" {
				if cfg.GoogleCredentialsFile != "" {
					slog.Info("Authenticating with Google Drive using dynamically refreshed token from file", "path", cfg.GoogleCredentialsFile)
					ts := newFileTokenSource(cfg.GoogleCredentialsFile)
					opts = append(opts, option.WithTokenSource(ts))
				} else {
					slog.Info("Authenticating with Google Drive using OAuth2 access token from credentials JSON")
					token := &oauth2.Token{AccessToken: tok}
					opts = append(opts, option.WithTokenSource(oauth2.StaticTokenSource(token)))
				}
			} else if _, ok := peek["refresh_token"]; ok {
				slog.Info("Authenticating with Google Drive using OAuth2 refresh token from credentials JSON")
				var tc oauthTokenConfig
				if err := json.Unmarshal(rawCreds, &tc); err != nil {
					return nil, fmt.Errorf("parsing oauth2 token config: %w", err)
				}
				oauthConf := &oauth2.Config{
					ClientID:     tc.ClientID,
					ClientSecret: tc.ClientSecret,
					Endpoint:     google.Endpoint,
					Scopes:       []string{drive.DriveScope},
				}
				token := &oauth2.Token{RefreshToken: tc.RefreshToken}
				opts = append(opts, option.WithTokenSource(oauthConf.TokenSource(ctx, token)))
			} else {
				opts = append(opts, option.WithAuthCredentialsJSON(option.AuthorizedUser, rawCreds))
			}
		}
	} else if cfg.GoogleRefreshToken != "" && cfg.GoogleClientID != "" {
		slog.Info("Authenticating with Google Drive using OAuth2 refresh token from environment")
		oauthConf := &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			Endpoint:     google.Endpoint,
			Scopes:       []string{drive.DriveScope},
		}
		token := &oauth2.Token{RefreshToken: cfg.GoogleRefreshToken}
		opts = append(opts, option.WithTokenSource(oauthConf.TokenSource(ctx, token)))
	} else {
		slog.Info("Falling back to Application Default Credentials (ADC)")
		opts = append(opts, option.WithScopes(drive.DriveScope))
	}

	srv, err := drive.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating google drive service: %w", err)
	}

	return &Client{service: srv}, nil
}

var takeoutRegex = regexp.MustCompile(`takeout-([0-9]{8}T[0-9]{6}Z)`)

// ListBatches discovers Takeout archives in the specified folder and groups them into chronological batches.
func (c *Client) ListBatches(ctx context.Context, folderID string) ([]TakeoutBatch, error) {
	files, err := c.collectArchiveFiles(ctx, folderID)
	if err != nil {
		return nil, err
	}

	batchMap := make(map[string]*TakeoutBatch)

	for _, file := range files {
		matches := takeoutRegex.FindStringSubmatch(file.Name)
		var batchID string
		var exportDate time.Time

		if len(matches) > 1 {
			batchID = matches[1]
			t, err := time.Parse("20060102T150405Z", batchID)
			if err == nil {
				exportDate = t
			}
		}

		if batchID == "" {
			// Fallback: derive batch from parent folder or timestamp prefix
			batchID = "batch-" + file.Name
			exportDate = time.Now()
		}

		batch, exists := batchMap[batchID]
		if !exists {
			batch = &TakeoutBatch{
				ID:         batchID,
				ExportDate: exportDate,
				Files:      nil,
			}
			batchMap[batchID] = batch
		}

		batch.Files = append(batch.Files, file)
		batch.TotalBytes += file.Size
	}

	batches := make([]TakeoutBatch, 0, len(batchMap))
	for _, batch := range batchMap {
		// Sort files within the batch deterministically by filename
		sort.Slice(batch.Files, func(i, j int) bool {
			return batch.Files[i].Name < batch.Files[j].Name
		})
		batches = append(batches, *batch)
	}

	// Sort batches chronologically: earliest exports (full snapshot) first
	sort.Slice(batches, func(i, j int) bool {
		if batches[i].ExportDate.Equal(batches[j].ExportDate) {
			return batches[i].ID < batches[j].ID
		}
		return batches[i].ExportDate.Before(batches[j].ExportDate)
	})

	return batches, nil
}

func (c *Client) collectArchiveFiles(ctx context.Context, folderID string) ([]FileInfo, error) {
	var results []FileInfo

	query := fmt.Sprintf("'%s' in parents and trashed = false", folderID)
	pageToken := ""

	for {
		call := c.service.Files.List().
			Q(query).
			Fields("nextPageToken, files(id, name, mimeType, size, md5Checksum, createdTime)").
			PageSize(100)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}

		fileList, err := call.Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("listing files in folder %s: %w", folderID, err)
		}

		for _, f := range fileList.Files {
			if f.MimeType == "application/vnd.google-apps.folder" {
				// Search nested folder recursively
				nested, err := c.collectArchiveFiles(ctx, f.Id)
				if err != nil {
					return nil, err
				}
				results = append(results, nested...)
				continue
			}

			// Only process archive files (.zip, .tgz)
			nameLower := strings.ToLower(f.Name)
			if strings.HasSuffix(nameLower, ".zip") || strings.HasSuffix(nameLower, ".tgz") {
				results = append(results, FileInfo{
					ID:          f.Id,
					Name:        f.Name,
					Size:        f.Size,
					MD5Checksum: f.Md5Checksum,
				})
			}
		}

		pageToken = fileList.NextPageToken
		if pageToken == "" {
			break
		}
	}

	return results, nil
}

// DownloadFile downloads a file from Google Drive to destPath and verifies its MD5 checksum.
// If the destination file already exists and matches expected size and MD5, the download is skipped.
// If an interrupted partial download exists (.downloading), it resumes downloading using HTTP Range requests.
// When an attempt is interrupted but forward progress was made, the failure counter is reset.
func (c *Client) DownloadFile(ctx context.Context, fileID string, destPath string, expectedMD5 string, expectedSize int64) error {
	destClean := filepath.Clean(destPath)
	if err := os.MkdirAll(filepath.Dir(destClean), 0750); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	if checkFileIntegrity(destClean, expectedMD5, expectedSize) {
		slog.Info("File already downloaded and verified, skipping download", "path", destClean)
		return nil
	}

	tmpPath := filepath.Clean(destClean + ".downloading")
	const maxFailuresWithoutProgress = 5
	consecutiveFailures := 0

	for {
		offsetBefore := getFileSize(tmpPath)
		if expectedSize > 0 && offsetBefore > expectedSize {
			_ = os.Remove(tmpPath)
			offsetBefore = 0
		}

		err := c.downloadRangeAttempt(ctx, fileID, destClean, tmpPath, offsetBefore, expectedMD5, expectedSize)
		if err == nil {
			return nil
		}

		offsetAfter := getFileSize(tmpPath)
		if offsetAfter > offsetBefore {
			slog.Info("Download attempt interrupted but progress was made; resetting error count",
				"file", filepath.Base(destClean),
				"bytesBefore", offsetBefore,
				"bytesAfter", offsetAfter,
				"advancedBytes", offsetAfter-offsetBefore,
			)
			consecutiveFailures = 0
		} else {
			consecutiveFailures++
			slog.Warn("Download attempt failed without progress",
				"file", filepath.Base(destClean),
				"consecutiveFailures", consecutiveFailures,
				"maxFailures", maxFailuresWithoutProgress,
				"error", err,
			)
		}

		if consecutiveFailures >= maxFailuresWithoutProgress {
			return fmt.Errorf("download of %s failed after %d attempts without progress: %w", destClean, consecutiveFailures, err)
		}

		backoff := time.Duration(1<<consecutiveFailures) * time.Second
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

func (c *Client) downloadRangeAttempt(ctx context.Context, fileID string, destClean, tmpPath string, existingSize int64, expectedMD5 string, expectedSize int64) error {
	call := c.service.Files.Get(fileID).Context(ctx)

	// #nosec G401 -- MD5 is required for comparison against Google Drive API's md5Checksum
	hasher := md5.New()

	var tmpFile *os.File
	var err error

	if existingSize > 0 {
		// #nosec G304 -- tmpPath is cleaned path derived from validated target
		existingFile, oErr := os.Open(tmpPath)
		if oErr != nil {
			return fmt.Errorf("opening partial download for hashing: %w", oErr)
		}
		if _, cpErr := io.Copy(hasher, existingFile); cpErr != nil {
			_ = existingFile.Close()
			return fmt.Errorf("hashing partial download: %w", cpErr)
		}
		_ = existingFile.Close()

		slog.Info("Resuming partial download", "path", destClean, "existingBytes", existingSize, "expectedBytes", expectedSize)
		call.Header().Set("Range", fmt.Sprintf("bytes=%d-", existingSize))

		// #nosec G304 -- tmpPath is cleaned path derived from validated target
		tmpFile, err = os.OpenFile(tmpPath, os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return fmt.Errorf("opening temp file for append %s: %w", tmpPath, err)
		}
	} else {
		slog.Info("Downloading archive part", "id", fileID, "dest", destClean, "sizeBytes", expectedSize)
		// #nosec G304 -- tmpPath is cleaned path derived from validated target
		tmpFile, err = os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			return fmt.Errorf("creating temp file %s: %w", tmpPath, err)
		}
	}
	defer func() {
		_ = tmpFile.Close()
	}()

	res, err := call.Download()
	if err != nil {
		return fmt.Errorf("initiating download for %s: %w", fileID, err)
	}
	defer func() {
		_ = res.Body.Close()
	}()

	// If server ignored Range and returned 200 OK, reset to byte 0
	if existingSize > 0 && res.StatusCode == http.StatusOK {
		slog.Warn("Server ignored Range header, restarting download from byte 0", "path", destClean)
		if _, skErr := tmpFile.Seek(0, io.SeekStart); skErr != nil {
			return fmt.Errorf("seeking to start of file: %w", skErr)
		}
		if trErr := tmpFile.Truncate(0); trErr != nil {
			return fmt.Errorf("truncating file: %w", trErr)
		}
		hasher.Reset()
		existingSize = 0
	}

	writer := io.MultiWriter(tmpFile, hasher)
	written, err := io.Copy(writer, res.Body)
	_ = tmpFile.Sync()
	if written > 0 && c.metrics != nil {
		c.metrics.BytesDownloadedTotal.Add(float64(written))
	}
	if err != nil {
		return fmt.Errorf("downloading file stream (at offset %d): %w", existingSize, err)
	}

	totalDownloaded := existingSize + written
	if expectedSize > 0 && totalDownloaded != expectedSize {
		return fmt.Errorf("size mismatch for %s: expected %d bytes, got %d", destClean, expectedSize, totalDownloaded)
	}

	computedMD5 := hex.EncodeToString(hasher.Sum(nil))
	if expectedMD5 != "" && computedMD5 != expectedMD5 {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("md5 mismatch for %s: expected %s, got %s", destClean, expectedMD5, computedMD5)
	}

	if err := os.Rename(tmpPath, destClean); err != nil {
		return fmt.Errorf("renaming completed download: %w", err)
	}

	slog.Info("Download completed successfully", "path", destClean, "bytes", totalDownloaded)
	return nil
}

func getFileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return 0
	}
	return info.Size()
}

type fileTokenSource struct {
	path  string
	email string
	mu    sync.Mutex
	tok   *oauth2.Token
}

func newFileTokenSource(path string) *fileTokenSource {
	return &fileTokenSource{path: path}
}

func (s *fileTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if s.tok != nil && s.tok.Expiry.After(now.Add(2*time.Minute)) {
		return s.tok, nil
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("reading token file %s: %w", s.path, err)
	}

	var parsed struct {
		AccessToken          string `json:"access_token"`
		AccessTokenExpiresAt string `json:"access_token_expires_at"`
		Email                string `json:"email"`
	}
	if err := json.Unmarshal(data, &parsed); err == nil && parsed.AccessToken != "" {
		if s.email == "" && parsed.Email != "" {
			s.email = parsed.Email
		}
		expiry, pErr := time.Parse(time.RFC3339, parsed.AccessTokenExpiresAt)
		if pErr == nil && expiry.After(now.Add(2*time.Minute)) {
			s.tok = &oauth2.Token{
				AccessToken: parsed.AccessToken,
				Expiry:      expiry,
			}
			return s.tok, nil
		}
	}

	// Try refreshing via gog cli if available
	if s.email != "" {
		if gogPath, lErr := exec.LookPath("gog"); lErr == nil && gogPath != "" {
			slog.Info("Token expiring soon or expired, refreshing via gog", "email", s.email, "path", s.path)
			// Trigger online refresh within gog
			// #nosec G204 -- parameters are verified local path and user email
			_ = exec.Command(gogPath, "drive", "ls", "-a", s.email, "--max", "1").Run()
			// Export the refreshed token
			// #nosec G204
			cmd := exec.Command(gogPath, "auth", "tokens", "export", s.email, "--out", s.path, "--overwrite")
			if out, cErr := cmd.CombinedOutput(); cErr != nil {
				slog.Warn("Failed to refresh token via gog", "error", cErr, "output", string(out))
			} else {
				refreshedData, rErr := os.ReadFile(s.path)
				if rErr == nil {
					var rParsed struct {
						AccessToken          string `json:"access_token"`
						AccessTokenExpiresAt string `json:"access_token_expires_at"`
					}
					if jErr := json.Unmarshal(refreshedData, &rParsed); jErr == nil && rParsed.AccessToken != "" {
						rExpiry, rpErr := time.Parse(time.RFC3339, rParsed.AccessTokenExpiresAt)
						if rpErr == nil {
							s.tok = &oauth2.Token{
								AccessToken: rParsed.AccessToken,
								Expiry:      rExpiry,
							}
							slog.Info("Refreshed OAuth2 token via gog", "expiresAt", rParsed.AccessTokenExpiresAt)
							return s.tok, nil
						}
					}
				}
			}
		}
	}

	if s.tok != nil && s.tok.Valid() {
		return s.tok, nil
	}
	return nil, fmt.Errorf("no valid OAuth2 access token available in %s", s.path)
}

// TrashFile moves a file to Google Drive trash (soft-delete).
func (c *Client) TrashFile(ctx context.Context, fileID string) error {
	slog.Info("Moving file to Google Drive trash", "id", fileID)
	update := &drive.File{Trashed: true}
	_, err := c.service.Files.Update(fileID, update).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("trashing file %s: %w", fileID, err)
	}
	return nil
}

func checkFileIntegrity(path string, expectedMD5 string, expectedSize int64) bool {
	cleanedPath := filepath.Clean(path)
	info, err := os.Stat(cleanedPath)
	if err != nil || info.IsDir() {
		return false
	}
	if expectedSize > 0 && info.Size() != expectedSize {
		return false
	}
	if expectedMD5 == "" {
		return true
	}

	f, err := os.Open(cleanedPath)
	if err != nil {
		return false
	}
	defer func() {
		_ = f.Close()
	}()

	// #nosec G401 -- MD5 is required for comparison against Google Drive API's md5Checksum
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}

	return hex.EncodeToString(h.Sum(nil)) == expectedMD5
}
