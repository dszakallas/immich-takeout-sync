package drive

import (
	// #nosec G501 -- MD5 is required for comparison against Google Drive API's md5Checksum
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTakeoutRegex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		filename   string
		wantMatch  bool
		wantBatch  string
		wantParsed time.Time
	}{
		{
			name:       "standard takeout split file part 1",
			filename:   "takeout-20260901T120000Z-001.zip",
			wantMatch:  true,
			wantBatch:  "20260901T120000Z",
			wantParsed: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		},
		{
			name:       "standard takeout split file part 2",
			filename:   "takeout-20260901T120000Z-002.zip",
			wantMatch:  true,
			wantBatch:  "20260901T120000Z",
			wantParsed: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		},
		{
			name:       "single archive takeout",
			filename:   "takeout-20261015T083000Z.zip",
			wantMatch:  true,
			wantBatch:  "20261015T083000Z",
			wantParsed: time.Date(2026, 10, 15, 8, 30, 0, 0, time.UTC),
		},
		{
			name:      "unrelated file",
			filename:  "photos_backup.zip",
			wantMatch: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			matches := takeoutRegex.FindStringSubmatch(tc.filename)
			if tc.wantMatch {
				if len(matches) < 2 {
					t.Fatalf("expected match for %s, got none", tc.filename)
				}
				if matches[1] != tc.wantBatch {
					t.Errorf("batch ID = %q, want %q", matches[1], tc.wantBatch)
				}
				parsed, err := time.Parse("20060102T150405Z", matches[1])
				if err != nil {
					t.Fatalf("failed to parse timestamp: %v", err)
				}
				if !parsed.Equal(tc.wantParsed) {
					t.Errorf("parsed time = %v, want %v", parsed, tc.wantParsed)
				}
			} else {
				if len(matches) > 0 {
					t.Errorf("expected no match for %s, got %v", tc.filename, matches)
				}
			}
		})
	}
}

func TestCheckFileIntegrity(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	content := []byte("google-photos-takeout-test-data")
	testFile := filepath.Join(tmpDir, "test.zip")
	if err := os.WriteFile(testFile, content, 0600); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// #nosec G401 -- MD5 is required for comparison against Google Drive API's md5Checksum
	h := md5.New()
	h.Write(content)
	correctMD5 := hex.EncodeToString(h.Sum(nil))
	correctSize := int64(len(content))

	tests := []struct {
		name         string
		path         string
		expectedMD5  string
		expectedSize int64
		wantValid    bool
	}{
		{
			name:         "exact match",
			path:         testFile,
			expectedMD5:  correctMD5,
			expectedSize: correctSize,
			wantValid:    true,
		},
		{
			name:         "match with empty expectedMD5",
			path:         testFile,
			expectedMD5:  "",
			expectedSize: correctSize,
			wantValid:    true,
		},
		{
			name:         "wrong size",
			path:         testFile,
			expectedMD5:  correctMD5,
			expectedSize: correctSize + 10,
			wantValid:    false,
		},
		{
			name:         "wrong md5",
			path:         testFile,
			expectedMD5:  "00000000000000000000000000000000",
			expectedSize: correctSize,
			wantValid:    false,
		},
		{
			name:         "non-existent file",
			path:         filepath.Join(tmpDir, "missing.zip"),
			expectedMD5:  correctMD5,
			expectedSize: correctSize,
			wantValid:    false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := checkFileIntegrity(tc.path, tc.expectedMD5, tc.expectedSize)
			if got != tc.wantValid {
				t.Errorf("checkFileIntegrity() = %v, want %v", got, tc.wantValid)
			}
		})
	}
}
