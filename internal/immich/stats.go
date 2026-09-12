package immich

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ErroredAsset holds details of an individual asset that failed to upload.
type ErroredAsset struct {
	File   string
	Reason string
	Error  string
}

// ImportStats aggregates the results of an immich-go execution.
type ImportStats struct {
	TotalAssets  int
	Processed    int
	Discarded    int
	Errors       int
	Pending      int
	Uploaded     int
	Upgraded     int
	ErrorReasons map[string]int
	ErroredFiles []ErroredAsset
	HasReport    bool
	HasErrorFlag bool
	LogFile      string
}

var (
	reLogFile     = regexp.MustCompile(`Log file:\s*(.+)`)
	reTotalAssets = regexp.MustCompile(`Total Assets:\s+(\d+)`)
	reProcessed   = regexp.MustCompile(`\s*Processed:\s+(\d+)`)
	reDiscarded   = regexp.MustCompile(`\s*Discarded:\s+(\d+)`)
	reErrors      = regexp.MustCompile(`\s*Errors:\s+(\d+)`)
	rePending     = regexp.MustCompile(`\s*Pending:\s+(\d+)`)

	reUploaded = regexp.MustCompile(`uploaded successfully\s*:\s*(\d+)`)
	reUpgraded = regexp.MustCompile(`server asset upgraded\s*:\s*(\d+)`)

	reLifecycleError = regexp.MustCompile(`(server error|upload failed|file access error|incomplete)\s*:\s*(\d+)`)
	reFileError      = regexp.MustCompile(`ERR\s+(server error|upload failed|file access error)\s+file=(.+?)\s+error=(.+)`)
)

// ParseOutputLine extracts statistics and error entries from an immich-go output line.
func ParseOutputLine(line string, stats *ImportStats) {
	if stats.ErrorReasons == nil {
		stats.ErrorReasons = make(map[string]int)
	}

	trimmed := strings.TrimSpace(line)

	if strings.Contains(trimmed, "Some errors have occurred. Look at the log file for details") {
		stats.HasErrorFlag = true
	}

	if m := reTotalAssets.FindStringSubmatch(trimmed); len(m) > 1 {
		stats.TotalAssets, _ = strconv.Atoi(m[1])
		stats.HasReport = true
	} else if m := reProcessed.FindStringSubmatch(trimmed); len(m) > 1 {
		stats.Processed, _ = strconv.Atoi(m[1])
	} else if m := reDiscarded.FindStringSubmatch(trimmed); len(m) > 1 {
		stats.Discarded, _ = strconv.Atoi(m[1])
	} else if m := reErrors.FindStringSubmatch(trimmed); len(m) > 1 {
		stats.Errors, _ = strconv.Atoi(m[1])
	} else if m := rePending.FindStringSubmatch(trimmed); len(m) > 1 {
		stats.Pending, _ = strconv.Atoi(m[1])
	} else if m := reUploaded.FindStringSubmatch(trimmed); len(m) > 1 {
		stats.Uploaded, _ = strconv.Atoi(m[1])
	} else if m := reUpgraded.FindStringSubmatch(trimmed); len(m) > 1 {
		stats.Upgraded, _ = strconv.Atoi(m[1])
	} else if m := reLifecycleError.FindStringSubmatch(trimmed); len(m) > 2 {
		reason := strings.ReplaceAll(strings.TrimSpace(m[1]), " ", "_")
		cnt, _ := strconv.Atoi(m[2])
		stats.ErrorReasons[reason] = cnt
	} else if m := reLogFile.FindStringSubmatch(trimmed); len(m) > 1 {
		stats.LogFile = strings.TrimSpace(m[1])
	} else if m := reFileError.FindStringSubmatch(trimmed); len(m) > 3 {
		reason := strings.ReplaceAll(strings.TrimSpace(m[1]), " ", "_")
		stats.ErroredFiles = append(stats.ErroredFiles, ErroredAsset{
			Reason: reason,
			File:   m[2],
			Error:  m[3],
		})
	}
}

// ParseLogFile reads an immich-go log file and parses its contents into stats.
func ParseLogFile(path string, stats *ImportStats) error {
	// #nosec G304 -- path is captured from trusted immich-go output
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
	}()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		ParseOutputLine(scanner.Text(), stats)
	}
	return scanner.Err()
}

// IsPartialAssetError returns true if the execution completed the upload phase
// but reported individual asset errors (which shouldn't fail the entire job).
func (s *ImportStats) IsPartialAssetError() bool {
	if s == nil {
		return false
	}
	// If immich-go produced an asset tracking report and processed at least one asset
	// or logged asset errors, it was an asset-level issue rather than a fatal crash.
	if s.HasReport && (s.Errors > 0 || s.Processed > 0 || len(s.ErroredFiles) > 0) {
		return true
	}
	if s.HasErrorFlag && (s.Processed > 0 || s.Errors > 0) {
		return true
	}
	return false
}
