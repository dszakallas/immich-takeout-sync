// Package immich provides execution capabilities for the immich-go CLI tool.
package immich

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"

	"github.com/dszakallas/immich-takeout-sync/internal/config"
)

// Runner invokes the immich-go CLI binary.
type Runner struct {
	cfg *config.Config
}

// NewRunner creates a new immich-go runner instance.
func NewRunner(cfg *config.Config) *Runner {
	return &Runner{cfg: cfg}
}

// ImportBatch runs immich-go upload from-google-photos for the specified zip files and optional metadata directory.
// Non-fatal asset errors are captured in ImportStats and logged as warnings rather than failing the execution.
func (r *Runner) ImportBatch(ctx context.Context, metadataDir string, archivePaths []string) (*ImportStats, error) {
	if len(archivePaths) == 0 {
		return nil, fmt.Errorf("no archive paths provided for import")
	}

	if r.cfg.DryRun {
		slog.InfoContext(ctx, "[DRY-RUN] Simulating immich-go upload", "files", archivePaths, "metadataDir", metadataDir)
		return &ImportStats{Processed: len(archivePaths)}, nil
	}

	args := []string{
		"upload",
		"from-google-photos",
		fmt.Sprintf("--server=%s", r.cfg.ImmichServerURL),
		fmt.Sprintf("--api-key=%s", r.cfg.ImmichAPIKey),
		"--no-ui",
		"--on-errors=continue",
		"--sync-albums=true",
		"--takeout-tag=true",
		"--people-tag=true",
		"--include-archived=true",
	}
	if metadataDir != "" {
		args = append(args, metadataDir)
	}
	args = append(args, archivePaths...)

	cmdName := r.cfg.ImmichGoBinary
	if cmdName == "" {
		cmdName = "immich-go"
	}

	slog.InfoContext(ctx, "Executing immich-go", "bin", cmdName, "filesCount", len(archivePaths), "server", r.cfg.ImmichServerURL)
	// #nosec G204 -- immich-go binary name and arguments are constructed from trusted local config
	cmd := exec.CommandContext(ctx, cmdName, args...)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting immich-go: %w", err)
	}

	var (
		mu    sync.Mutex
		stats ImportStats
		wg    sync.WaitGroup
	)

	readPipe := func(reader io.Reader, level slog.Level) {
		defer wg.Done()
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			slog.Log(ctx, level, "immich-go", "msg", trimmed)
			mu.Lock()
			ParseOutputLine(trimmed, &stats)
			mu.Unlock()
		}
	}

	wg.Add(2)
	go readPipe(stdoutPipe, slog.LevelInfo)
	go readPipe(stderrPipe, slog.LevelWarn)

	waitErr := cmd.Wait()
	wg.Wait()

	if stats.LogFile != "" && stats.TotalAssets == 0 {
		if err := ParseLogFile(stats.LogFile, &stats); err != nil {
			slog.WarnContext(ctx, "Failed to parse immich-go log file", "path", stats.LogFile, "err", err)
		}
	}

	if waitErr != nil {
		if stats.IsPartialAssetError() {
			slog.WarnContext(ctx, "immich-go finished with non-fatal asset import errors",
				"totalAssets", stats.TotalAssets,
				"processed", stats.Processed,
				"errors", stats.Errors,
				"discarded", stats.Discarded,
				"erroredFilesCount", len(stats.ErroredFiles),
			)
			for _, ef := range stats.ErroredFiles {
				slog.WarnContext(ctx, "Individual asset import failed", "file", ef.File, "reason", ef.Reason, "error", ef.Error)
			}
			return &stats, nil
		}
		return &stats, fmt.Errorf("immich-go failed with fatal error: %w", waitErr)
	}

	slog.InfoContext(ctx, "immich-go upload finished successfully",
		"totalAssets", stats.TotalAssets,
		"processed", stats.Processed,
		"uploaded", stats.Uploaded,
		"upgraded", stats.Upgraded,
		"discarded", stats.Discarded,
	)
	return &stats, nil
}
