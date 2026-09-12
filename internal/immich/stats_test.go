package immich

import (
	"testing"
)

func TestParseOutputLine(t *testing.T) {
	t.Parallel()

	sampleOutput := `
2026-05-08 23:50:54 ERR server error file=Takeout:Google Photos/Bali/sample.jpg error=AssetUpload, POST, https://immich/api/assets
2026-05-08 23:50:54 ERR upload failed file=Takeout:Google Photos/Bali/sample2.mp4 error=network timeout
Asset Tracking Report:
=====================
Total Assets:        100  (10.5 GB)
  Processed:          95  (9.8 GB)
  Discarded:           2  (100 MB)
  Errors:              3  (600 MB)
  Pending:             0  (0 B)

Event Report:
=============
Asset Lifecycle (PROCESSED):
  uploaded successfully              :      90  (9.0 GB)
  server asset upgraded              :       5  (800 MB)
Asset Lifecycle (ERROR):
  server error                       :       2
  upload failed                      :       1
Some errors have occurred. Look at the log file for details
`

	var stats ImportStats
	for _, line := range splitLines(sampleOutput) {
		ParseOutputLine(line, &stats)
	}

	if stats.TotalAssets != 100 {
		t.Errorf("expected 100 TotalAssets, got %d", stats.TotalAssets)
	}
	if stats.Processed != 95 {
		t.Errorf("expected 95 Processed, got %d", stats.Processed)
	}
	if stats.Discarded != 2 {
		t.Errorf("expected 2 Discarded, got %d", stats.Discarded)
	}
	if stats.Errors != 3 {
		t.Errorf("expected 3 Errors, got %d", stats.Errors)
	}
	if stats.Uploaded != 90 {
		t.Errorf("expected 90 Uploaded, got %d", stats.Uploaded)
	}
	if stats.Upgraded != 5 {
		t.Errorf("expected 5 Upgraded, got %d", stats.Upgraded)
	}
	if stats.ErrorReasons["server_error"] != 2 {
		t.Errorf("expected 2 server_error, got %d", stats.ErrorReasons["server_error"])
	}
	if stats.ErrorReasons["upload_failed"] != 1 {
		t.Errorf("expected 1 upload_failed, got %d", stats.ErrorReasons["upload_failed"])
	}
	if len(stats.ErroredFiles) != 2 {
		t.Errorf("expected 2 ErroredFiles, got %d", len(stats.ErroredFiles))
	}
	if !stats.IsPartialAssetError() {
		t.Errorf("expected IsPartialAssetError to be true")
	}
}

func splitLines(s string) []string {
	var lines []string
	curr := ""
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, curr)
			curr = ""
		} else {
			curr += string(s[i])
		}
	}
	if curr != "" {
		lines = append(lines, curr)
	}
	return lines
}
