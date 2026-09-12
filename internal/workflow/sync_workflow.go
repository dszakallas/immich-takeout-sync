// Package workflow implements the durable DBOS orchestration pipeline for syncing Takeout exports.
package workflow

import (
	"archive/zip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dszakallas/immich-takeout-sync/internal/config"
	"github.com/dszakallas/immich-takeout-sync/internal/drive"
	"github.com/dszakallas/immich-takeout-sync/internal/immich"
	"github.com/dszakallas/immich-takeout-sync/internal/metrics"
	// Register database drivers for database/sql and DBOS (SQLite and PostgreSQL).
	_ "github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Orchestrator coordinates the Takeout synchronization pipeline using DBOS.
type Orchestrator struct {
	cfg         *config.Config
	driveClient *drive.Client
	immichRun   *immich.Runner
	metrics     *metrics.Metrics
	dbosCtx     dbos.Context
	db          *sql.DB
}

// openDatabase initializes a database connection matching the DBOS database URL.
func openDatabase(databaseURL string) (*sql.DB, error) {
	if strings.HasPrefix(databaseURL, "sqlite:") {
		dbPath := strings.TrimPrefix(databaseURL, "sqlite:")
		dbPath = strings.TrimPrefix(dbPath, "//")
		cleanedDBPath := filepath.Clean(dbPath)
		if cleanedDBPath != "" && cleanedDBPath != ":memory:" {
			if err := os.MkdirAll(filepath.Dir(cleanedDBPath), 0750); err != nil {
				return nil, fmt.Errorf("creating database directory: %w", err)
			}
		}
		return sql.Open("sqlite", cleanedDBPath)
	}

	if strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://") {
		return sql.Open("pgx", databaseURL)
	}

	driver := "sqlite"
	if idx := strings.Index(databaseURL, ":"); idx != -1 {
		scheme := databaseURL[:idx]
		if scheme == "postgres" || scheme == "postgresql" {
			driver = "pgx"
		}
	}
	return sql.Open(driver, databaseURL)
}

// NewOrchestrator creates a new pipeline orchestrator.
func NewOrchestrator(ctx context.Context, cfg *config.Config, driveClient *drive.Client, immichRun *immich.Runner, m *metrics.Metrics) (*Orchestrator, error) {
	db, err := openDatabase(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("opening application database: %w", err)
	}

	if err := initSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing database schema: %w", err)
	}

	dbosCtx, err := dbos.NewContext(ctx, dbos.Config{
		DatabaseURL: cfg.DatabaseURL,
		AppName:     "immich-takeout-sync",
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing DBOS context: %w", err)
	}

	orch := &Orchestrator{
		cfg:         cfg,
		driveClient: driveClient,
		immichRun:   immichRun,
		metrics:     m,
		dbosCtx:     dbosCtx,
		db:          db,
	}

	dbos.RegisterWorkflow(dbosCtx, orch.SyncPipelineWorkflow)

	return orch, nil
}

// Start launches DBOS and executes the synchronization pipeline.
func (o *Orchestrator) Start(ctx context.Context) error {
	if err := dbos.Launch(o.dbosCtx); err != nil {
		return fmt.Errorf("launching DBOS: %w", err)
	}
	defer func() {
		_ = dbos.Shutdown(o.dbosCtx, 5*time.Second)
	}()

	workflowID := fmt.Sprintf("takeout-sync-%s", time.Now().UTC().Format("20060102-150405"))
	handle, err := dbos.RunWorkflow(o.dbosCtx, o.SyncPipelineWorkflow, o.cfg.GoogleDriveFolderID, dbos.WithWorkflowID(workflowID))
	if err != nil {
		return fmt.Errorf("starting sync workflow: %w", err)
	}

	slog.InfoContext(ctx, "Sync workflow started", "workflowID", workflowID)
	result, err := handle.GetResult()
	if err != nil {
		return fmt.Errorf("sync workflow failed: %w", err)
	}

	slog.InfoContext(ctx, "Sync workflow completed", "result", result)
	return nil
}

// Close releases open database handles.
func (o *Orchestrator) Close() error {
	if o.db != nil {
		return o.db.Close()
	}
	return nil
}

func initSchema(ctx context.Context, db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS export_batches (
		batch_id TEXT PRIMARY KEY,
		export_date TIMESTAMP NOT NULL,
		status TEXT NOT NULL,
		part_count INTEGER NOT NULL,
		total_bytes BIGINT NOT NULL,
		error_message TEXT,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS batch_files (
		drive_file_id TEXT PRIMARY KEY,
		batch_id TEXT NOT NULL REFERENCES export_batches(batch_id),
		file_name TEXT NOT NULL,
		file_size BIGINT NOT NULL,
		md5_checksum TEXT NOT NULL,
		is_downloaded BOOLEAN DEFAULT FALSE,
		is_trashed BOOLEAN DEFAULT FALSE,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	`
	_, err := db.ExecContext(ctx, schema)
	return err
}

// SyncPipelineWorkflow is the main durable DBOS workflow.
func (o *Orchestrator) SyncPipelineWorkflow(wCtx dbos.Context, folderID string) (string, error) {
	// Step 1: Discover batches in Google Drive
	batches, err := dbos.RunAsStep(wCtx, func(ctx context.Context) ([]drive.TakeoutBatch, error) {
		slog.InfoContext(ctx, "Step: Discovering Takeout archives in Google Drive", "folderID", folderID)
		allBatches, err := o.driveClient.ListBatches(ctx, folderID)
		if err != nil {
			return nil, err
		}

		var pending []drive.TakeoutBatch
		for _, b := range allBatches {
			completed, err := o.isBatchCompleted(ctx, b.ID)
			if err != nil {
				return nil, err
			}
			if completed {
				slog.InfoContext(ctx, "Batch already processed and completed, skipping", "batchID", b.ID)
				continue
			}
			pending = append(pending, b)
		}

		slog.InfoContext(ctx, "Discovery completed", "totalBatches", len(allBatches), "pendingBatches", len(pending))
		return pending, nil
	}, dbos.WithStepName("DiscoverBatches"))

	if err != nil {
		return "", fmt.Errorf("discovering batches: %w", err)
	}

	if len(batches) == 0 {
		return "No new or pending Takeout batches found", nil
	}

	// Two-phase processing for each batch (Phase 1: Metadata extraction, Phase 2: Sequential import)
	for _, batch := range batches {
		b := batch
		batchStart := time.Now()
		slog.Info("Starting processing for batch", "batchID", b.ID, "parts", len(b.Files), "totalBytes", b.TotalBytes)

		// Record initial status
		_, err := dbos.RunAsStep(wCtx, func(ctx context.Context) (bool, error) {
			return true, o.recordBatchStatus(ctx, b, "PROCESSING", "")
		}, dbos.WithStepName("RecordStatus-"+b.ID))
		if err != nil {
			if o.metrics != nil {
				o.metrics.RecordBatchFailed(b.ID, time.Since(batchStart))
			}
			return "", err
		}

		// Phase 1: Extract all JSON metadata sidecars across all parts of the batch into metadataDir
		metadataDir, err := dbos.RunAsStep(wCtx, func(ctx context.Context) (string, error) {
			metaDir := filepath.Join(o.cfg.ScratchDir, b.ID, "metadata")
			zipsDir := filepath.Join(o.cfg.ScratchDir, b.ID, "zips")
			if err := os.MkdirAll(metaDir, 0750); err != nil {
				return "", fmt.Errorf("creating metadata dir: %w", err)
			}
			if err := os.MkdirAll(zipsDir, 0750); err != nil {
				return "", fmt.Errorf("creating zips dir: %w", err)
			}

			slog.InfoContext(ctx, "Phase 1: Extracting metadata sidecars for batch", "batchID", b.ID, "parts", len(b.Files))

			for idx, f := range b.Files {
				dest := filepath.Join(zipsDir, f.Name)
				// Check if already downloaded
				if !fileExists(dest, f.Size) {
					// Check disk usage before download; evict other cached zips if above watermark
					o.ensureDiskHeadroom(ctx, zipsDir, f.Size)

					slog.InfoContext(ctx, "Downloading part for metadata extraction", "part", f.Name, "index", idx+1, "total", len(b.Files))
					if err := o.driveClient.DownloadFile(ctx, f.ID, dest, f.MD5Checksum, f.Size); err != nil {
						return "", fmt.Errorf("downloading file %s: %w", f.Name, err)
					}
					if o.metrics != nil {
						o.metrics.FilesDownloaded.Inc()
						if usage, uErr := getDiskUsageFraction(o.cfg.ScratchDir); uErr == nil {
							o.metrics.DiskUsageRatio.Set(usage)
						}
					}
				}

				extracted, err := extractJSONSidecars(dest, metaDir)
				if err != nil {
					return "", fmt.Errorf("extracting json from %s: %w", f.Name, err)
				}
				slog.InfoContext(ctx, "Extracted json sidecars from part", "file", f.Name, "extractedJSONs", extracted)

				// After extracting metadata, if disk usage exceeds high watermark, evict this zip
				if o.isDiskAboveWatermark(o.cfg.ScratchDir) {
					slog.InfoContext(ctx, "Disk usage above watermark, evicting cached zip", "file", f.Name)
					_ = os.Remove(dest)
				}
			}

			return metaDir, nil
		}, dbos.WithStepName("ExtractMetadata-"+b.ID), dbos.WithStepMaxRetries(3))
		if err != nil {
			_ = o.recordBatchStatus(context.Background(), b, "FAILED", err.Error())
			if o.metrics != nil {
				o.metrics.RecordBatchFailed(b.ID, time.Since(batchStart))
			}
			return "", fmt.Errorf("extracting metadata for batch %s: %w", b.ID, err)
		}

		// Phase 2: Sequential ingestion of each part paired with consolidated metadata
		_, err = dbos.RunAsStep(wCtx, func(ctx context.Context) (bool, error) {
			zipsDir := filepath.Join(o.cfg.ScratchDir, b.ID, "zips")
			slog.InfoContext(ctx, "Phase 2: Sequential import of batch parts into Immich", "batchID", b.ID, "parts", len(b.Files))

			for idx, f := range b.Files {
				dest := filepath.Join(zipsDir, f.Name)
				if !fileExists(dest, f.Size) {
					o.ensureDiskHeadroom(ctx, zipsDir, f.Size)
					slog.InfoContext(ctx, "Downloading part for import", "part", f.Name, "index", idx+1, "total", len(b.Files))
					if err := o.driveClient.DownloadFile(ctx, f.ID, dest, f.MD5Checksum, f.Size); err != nil {
						return false, fmt.Errorf("downloading part %s: %w", f.Name, err)
					}
					if o.metrics != nil {
						o.metrics.FilesDownloaded.Inc()
						if usage, uErr := getDiskUsageFraction(o.cfg.ScratchDir); uErr == nil {
							o.metrics.DiskUsageRatio.Set(usage)
						}
					}
				}

				slog.InfoContext(ctx, "Importing part into Immich", "file", f.Name, "part", idx+1, "total", len(b.Files))
				stats, err := o.immichRun.ImportBatch(ctx, metadataDir, []string{dest})
				if err != nil {
					return false, fmt.Errorf("importing part %s: %w", f.Name, err)
				}

				if o.metrics != nil && stats != nil {
					if stats.Uploaded > 0 {
						o.metrics.ImportedAssetsTotal.WithLabelValues(b.ID, "success").Add(float64(stats.Uploaded))
					}
					if stats.Upgraded > 0 {
						o.metrics.ImportedAssetsTotal.WithLabelValues(b.ID, "upgraded").Add(float64(stats.Upgraded))
					}
					if stats.Discarded > 0 {
						o.metrics.ImportedAssetsTotal.WithLabelValues(b.ID, "discarded").Add(float64(stats.Discarded))
					}
					if stats.Errors > 0 {
						o.metrics.ImportedAssetsTotal.WithLabelValues(b.ID, "error").Add(float64(stats.Errors))
						for reason, count := range stats.ErrorReasons {
							o.metrics.AssetErrorsTotal.WithLabelValues(b.ID, reason).Add(float64(count))
						}
						if len(stats.ErrorReasons) == 0 {
							o.metrics.AssetErrorsTotal.WithLabelValues(b.ID, "unknown").Add(float64(stats.Errors))
						}
					}
				}

				// Immediately delete processed zip to reclaim disk space
				slog.InfoContext(ctx, "Part imported successfully, reclaiming disk space", "file", f.Name)
				_ = os.Remove(dest)
			}
			return true, nil
		}, dbos.WithStepName("SequentialImport-"+b.ID), dbos.WithStepMaxRetries(2))
		if err != nil {
			_ = o.recordBatchStatus(context.Background(), b, "FAILED", err.Error())
			if o.metrics != nil {
				o.metrics.RecordBatchFailed(b.ID, time.Since(batchStart))
			}
			return "", fmt.Errorf("sequential import for batch %s: %w", b.ID, err)
		}

		// Phase 3: Soft-delete files in Google Drive (move to trash) ONLY if enabled
		_, err = dbos.RunAsStep(wCtx, func(ctx context.Context) (bool, error) {
			if !o.cfg.DeleteFromDrive {
				slog.InfoContext(ctx, "Skipping Google Drive trash step (DeleteFromDrive is false)", "batchID", b.ID)
				return true, nil
			}

			if o.cfg.DryRun {
				slog.InfoContext(ctx, "[DRY-RUN] Simulating soft-delete in Google Drive for batch", "batchID", b.ID)
				return true, nil
			}

			slog.InfoContext(ctx, "Step: Soft-deleting batch files in Google Drive", "batchID", b.ID, "count", len(b.Files))
			for _, f := range b.Files {
				if err := o.driveClient.TrashFile(ctx, f.ID); err != nil {
					return false, fmt.Errorf("trashing file %s (%s): %w", f.Name, f.ID, err)
				}
			}
			return true, nil
		}, dbos.WithStepName("TrashBatch-"+b.ID), dbos.WithStepMaxRetries(3))
		if err != nil {
			_ = o.recordBatchStatus(context.Background(), b, "FAILED", err.Error())
			if o.metrics != nil {
				o.metrics.RecordBatchFailed(b.ID, time.Since(batchStart))
			}
			return "", fmt.Errorf("trashing batch %s files in Drive: %w", b.ID, err)
		}

		// Phase 4: Clean up local scratch disk
		_, err = dbos.RunAsStep(wCtx, func(ctx context.Context) (bool, error) {
			batchDir := filepath.Join(o.cfg.ScratchDir, b.ID)
			slog.InfoContext(ctx, "Step: Purging local scratch files", "dir", batchDir)
			if err := os.RemoveAll(batchDir); err != nil {
				slog.WarnContext(ctx, "Failed to remove scratch dir", "dir", batchDir, "error", err)
			}
			return true, nil
		}, dbos.WithStepName("CleanupScratch-"+b.ID))
		if err != nil {
			slog.Warn("Scratch cleanup step encountered error", "batchID", b.ID, "error", err)
		}

		// Phase 5: Mark batch completed
		_, err = dbos.RunAsStep(wCtx, func(ctx context.Context) (bool, error) {
			return true, o.recordBatchStatus(ctx, b, "COMPLETED", "")
		}, dbos.WithStepName("MarkCompleted-"+b.ID))
		if err != nil {
			if o.metrics != nil {
				o.metrics.RecordBatchFailed(b.ID, time.Since(batchStart))
			}
			return "", err
		}

		if o.metrics != nil {
			o.metrics.RecordBatchCompleted(b.ID, time.Since(batchStart))
		}
		slog.Info("Successfully processed and completed batch", "batchID", b.ID)
	}

	return fmt.Sprintf("Successfully processed %d batches", len(batches)), nil
}

func (o *Orchestrator) isBatchCompleted(ctx context.Context, batchID string) (bool, error) {
	var status string
	err := o.db.QueryRowContext(ctx, "SELECT status FROM export_batches WHERE batch_id = $1", batchID).Scan(&status)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return status == "COMPLETED", nil
}

func (o *Orchestrator) recordBatchStatus(ctx context.Context, b drive.TakeoutBatch, status string, errMsg string) error {
	query := `
	INSERT INTO export_batches (batch_id, export_date, status, part_count, total_bytes, error_message, updated_at)
	VALUES ($1, $2, $3, $4, $5, $6, CURRENT_TIMESTAMP)
	ON CONFLICT(batch_id) DO UPDATE SET
		status = excluded.status,
		error_message = excluded.error_message,
		updated_at = CURRENT_TIMESTAMP;
	`
	_, err := o.db.ExecContext(ctx, query, b.ID, b.ExportDate, status, len(b.Files), b.TotalBytes, errMsg)
	if err != nil {
		return fmt.Errorf("updating export_batches: %w", err)
	}

	for _, f := range b.Files {
		fileQuery := `
		INSERT INTO batch_files (drive_file_id, batch_id, file_name, file_size, md5_checksum, is_downloaded, is_trashed, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, CURRENT_TIMESTAMP)
		ON CONFLICT(drive_file_id) DO UPDATE SET
			is_downloaded = CASE WHEN excluded.is_downloaded THEN TRUE ELSE batch_files.is_downloaded END,
			is_trashed = CASE WHEN excluded.is_trashed THEN TRUE ELSE batch_files.is_trashed END,
			updated_at = CURRENT_TIMESTAMP;
		`
		isDownloaded := status == "COMPLETED" || status == "IMPORTED"
		isTrashed := status == "COMPLETED"
		_, err = o.db.ExecContext(ctx, fileQuery, f.ID, b.ID, f.Name, f.Size, f.MD5Checksum, isDownloaded, isTrashed)
		if err != nil {
			return fmt.Errorf("updating batch_files: %w", err)
		}
	}
	return nil
}

func fileExists(path string, expectedSize int64) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Size() == expectedSize
}

func (o *Orchestrator) isDiskAboveWatermark(path string) bool {
	usage, err := getDiskUsageFraction(path)
	if err != nil {
		return false
	}
	return usage >= o.cfg.DiskHighWatermark
}

func (o *Orchestrator) ensureDiskHeadroom(ctx context.Context, zipsDir string, neededBytes int64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(o.cfg.ScratchDir, &stat); err != nil {
		return
	}
	if stat.Bsize <= 0 {
		return
	}
	freeBytes := stat.Bavail * uint64(stat.Bsize) // #nosec G115 -- Bsize is checked positive
	usage := float64(stat.Blocks-stat.Bavail) / float64(stat.Blocks)

	if usage < o.cfg.DiskHighWatermark && (neededBytes <= 0 || (neededBytes >= 0 && freeBytes > uint64(neededBytes))) {
		return
	}

	entries, err := os.ReadDir(zipsDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".zip") {
			continue
		}
		p := filepath.Join(zipsDir, entry.Name())
		slog.InfoContext(ctx, "Evicting cached zip to maintain disk headroom", "path", p)
		_ = os.Remove(p)
		if !o.isDiskAboveWatermark(o.cfg.ScratchDir) {
			break
		}
	}
}

// extractJSONSidecars scans the zip file and extracts all .json files preserving relative paths into targetDir.
func extractJSONSidecars(zipPath, targetDir string) (int, error) {
	cleanZip := filepath.Clean(zipPath)
	r, err := zip.OpenReader(cleanZip)
	if err != nil {
		return 0, fmt.Errorf("opening zip file %s: %w", cleanZip, err)
	}
	defer func() {
		_ = r.Close()
	}()

	count := 0
	cleanTarget := filepath.Clean(targetDir)

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(f.Name), ".json") {
			continue
		}
		cleanName := filepath.Clean(f.Name)
		if strings.HasPrefix(cleanName, "..") || filepath.IsAbs(cleanName) {
			continue // Prevent directory traversal
		}
		destPath := filepath.Join(cleanTarget, cleanName)
		if !strings.HasPrefix(destPath, cleanTarget+string(filepath.Separator)) {
			continue // Prevent zip slip
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0750); err != nil {
			return count, fmt.Errorf("creating directory for %s: %w", destPath, err)
		}

		rc, err := f.Open()
		if err != nil {
			return count, fmt.Errorf("reading file %s in zip: %w", f.Name, err)
		}
		// #nosec G304 -- destPath is verified to be within cleanTarget
		out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			_ = rc.Close()
			return count, fmt.Errorf("creating destination file %s: %w", destPath, err)
		}
		// Protect against decompression bombs by limiting sidecar JSON copy to 50 MiB
		const maxJSONBytes = 50 * 1024 * 1024
		// #nosec G110 -- protected by LimitReader
		_, cpErr := io.Copy(out, io.LimitReader(rc, maxJSONBytes))
		_ = rc.Close()
		_ = out.Close()
		if cpErr != nil {
			return count, fmt.Errorf("copying json content to %s: %w", destPath, cpErr)
		}
		count++
	}
	return count, nil
}

// getDiskUsageFraction returns the used fraction (0.0 - 1.0) of the filesystem containing path.
func getDiskUsageFraction(path string) (float64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Blocks == 0 {
		return 0, nil
	}
	free := stat.Bavail
	used := stat.Blocks - free
	return float64(used) / float64(stat.Blocks), nil
}
