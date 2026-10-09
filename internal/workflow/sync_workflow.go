// Package workflow implements the durable DBOS orchestration pipeline for syncing Takeout exports.
package workflow

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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
	ds          *dbos.DataSource
}

// ConfigName returns the unique configuration name for the orchestrator instance in DBOS.
func (o *Orchestrator) ConfigName() string {
	return "sync-orchestrator"
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

	dbosConfig := dbos.Config{
		AppName: "immich-takeout-sync",
	}

	// Share database connection pool if using SQLite (Recommendation 3.3)
	isSQLite := strings.HasPrefix(cfg.DatabaseURL, "sqlite:") || (!strings.HasPrefix(cfg.DatabaseURL, "postgres://") && !strings.HasPrefix(cfg.DatabaseURL, "postgresql://"))
	if isSQLite {
		dbosConfig.SQLiteSystemDB = db
	} else {
		dbosConfig.DatabaseURL = cfg.DatabaseURL
	}

	dbosCtx, err := dbos.NewContext(ctx, dbosConfig)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing DBOS context: %w", err)
	}

	ds, err := dbos.NewDataSource(dbosCtx, db, dbos.WithDataSourceName("immich_sync"))
	if err != nil {
		_ = dbos.Shutdown(dbosCtx, 5*time.Second)
		_ = db.Close()
		return nil, fmt.Errorf("creating DBOS data source: %w", err)
	}

	orch := &Orchestrator{
		cfg:         cfg,
		driveClient: driveClient,
		immichRun:   immichRun,
		metrics:     m,
		dbosCtx:     dbosCtx,
		db:          db,
		ds:          ds,
	}

	// Register workflow on configured instance (Recommendation 2.1)
	dbos.RegisterWorkflow(dbosCtx, orch.SyncPipelineWorkflow, dbos.WithInstance(orch))

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
	handle, err := dbos.RunWorkflow(o.dbosCtx, o.SyncPipelineWorkflow, o.cfg.GoogleDriveFolderID,
		dbos.WithRunInstance(o),
		dbos.WithWorkflowID(workflowID),
	)
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

	CREATE TABLE IF NOT EXISTS batch_metadata_bundles (
		batch_id TEXT PRIMARY KEY REFERENCES export_batches(batch_id),
		bundle_data BYTEA NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS batch_metadata_slices (
		batch_id TEXT NOT NULL,
		file_id TEXT NOT NULL,
		slice_data BYTEA NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (batch_id, file_id)
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
		slog.Info("Starting processing for batch", "batchID", b.ID, "parts", len(b.Files), "totalBytes", b.TotalBytes)

		// Checkpoint batch start time in a step for deterministic duration calculation
		batchStartTime, err := dbos.RunAsStep(wCtx, func(ctx context.Context) (time.Time, error) {
			return time.Now(), nil
		}, dbos.WithStepName("BatchStart-"+b.ID))
		if err != nil {
			return "", fmt.Errorf("starting batch timer for %s: %w", b.ID, err)
		}

		// Record initial status via DBOS transaction (Recommendation 2.3)
		if err := o.recordBatchStatus(wCtx, b, "PROCESSING", "", "RecordStatus-"+b.ID); err != nil {
			return "", err
		}

		// Phase 1: Extract and checkpoint metadata slices incrementally per part
		for idx, file := range b.Files {
			f := file
			partNum := idx + 1
			extractStepName := fmt.Sprintf("ExtractMetadataSlice-%s-%d", b.ID, partNum)

			sliceBytes, err := dbos.RunAsStep(wCtx, func(ctx context.Context) ([]byte, error) {
				zipsDir := filepath.Join(o.cfg.ScratchDir, b.ID, "zips")
				if err := os.MkdirAll(zipsDir, 0750); err != nil {
					return nil, fmt.Errorf("creating zips dir: %w", err)
				}
				dest := filepath.Join(zipsDir, f.Name)
				if !fileExists(dest, f.Size) {
					o.ensureDiskHeadroom(ctx, zipsDir, f.Size)

					slog.InfoContext(ctx, "Downloading part for metadata extraction", "part", f.Name, "index", partNum, "total", len(b.Files))
					if err := o.driveClient.DownloadFile(ctx, f.ID, dest, f.MD5Checksum, f.Size); err != nil {
						if o.metrics != nil {
							o.metrics.RecordBatchFailed(b.ID, time.Since(batchStartTime))
						}
						return nil, fmt.Errorf("downloading file %s: %w", f.Name, err)
					}
					if o.metrics != nil {
						o.metrics.FilesDownloaded.Inc()
						if usage, uErr := getDiskUsageFraction(o.cfg.ScratchDir); uErr == nil {
							o.metrics.DiskUsageRatio.Set(usage)
						}
					}
				}

				sliceData, count, err := extractJSONSidecarsToTarGz(dest)
				if err != nil {
					if o.metrics != nil {
						o.metrics.RecordBatchFailed(b.ID, time.Since(batchStartTime))
					}
					return nil, fmt.Errorf("extracting json from %s: %w", f.Name, err)
				}
				slog.InfoContext(ctx, "Extracted metadata slice from part", "file", f.Name, "part", partNum, "sidecars", count, "sliceBytes", len(sliceData))

				// If disk usage exceeds high watermark, evict this cached zip
				if o.isDiskAboveWatermark(o.cfg.ScratchDir) {
					slog.InfoContext(ctx, "Disk usage above watermark, evicting cached zip", "file", f.Name)
					_ = os.Remove(dest)
				}

				return sliceData, nil
			}, dbos.WithStepName(extractStepName), dbos.WithStepMaxRetries(3))
			if err != nil {
				_ = o.recordBatchStatus(wCtx, b, "FAILED", err.Error(), "RecordFailed-"+extractStepName)
				return "", fmt.Errorf("extracting metadata slice for batch %s part %s: %w", b.ID, f.Name, err)
			}

			// Checkpoint metadata slice in database individually via DBOS transaction
			saveStepName := fmt.Sprintf("SaveMetadataSlice-%s-%d", b.ID, partNum)
			if err := o.saveMetadataSlice(wCtx, b.ID, f.ID, sliceBytes, saveStepName); err != nil {
				_ = o.recordBatchStatus(wCtx, b, "FAILED", err.Error(), "RecordFailed-"+saveStepName)
				return "", fmt.Errorf("saving metadata slice for batch %s part %s: %w", b.ID, f.Name, err)
			}
		}

		// Assemble consolidated metadata directory from checkpointed slices
		metadataDir := filepath.Join(o.cfg.ScratchDir, b.ID, "metadata")
		totalMetaBytes, err := dbos.RunAsStep(wCtx, func(ctx context.Context) (int64, error) {
			slog.InfoContext(ctx, "Assembling all metadata slices into consolidated directory", "batchID", b.ID)
			totalBytes, count, err := o.assembleMetadataSlices(ctx, b.ID, metadataDir)
			if err != nil {
				return 0, fmt.Errorf("assembling metadata slices: %w", err)
			}
			slog.InfoContext(ctx, "Metadata assembly complete", "batchID", b.ID, "totalFiles", count, "totalBytes", totalBytes)
			return totalBytes, nil
		}, dbos.WithStepName("AssembleMetadata-"+b.ID), dbos.WithStepMaxRetries(2))
		if err != nil {
			_ = o.recordBatchStatus(wCtx, b, "FAILED", err.Error(), "RecordFailed-AssembleMetadata-"+b.ID)
			return "", fmt.Errorf("assembling metadata for batch %s: %w", b.ID, err)
		}
		slog.Info("Consolidated metadata size recorded", "batchID", b.ID, "bytes", totalMetaBytes)

		// Phase 2: Per-part sequential ingestion into Immich (Recommendation 3.1)
		for idx, file := range b.Files {
			f := file
			partNum := idx + 1
			stepName := fmt.Sprintf("ImportPart-%s-%d", b.ID, partNum)

			_, err = dbos.RunAsStep(wCtx, func(ctx context.Context) (bool, error) {
				// Guarantee metadata availability on resumed execution (Approach B)
				if err := o.ensureMetadataOnDisk(ctx, b.ID, metadataDir); err != nil {
					return false, fmt.Errorf("ensuring metadata available: %w", err)
				}

				zipsDir := filepath.Join(o.cfg.ScratchDir, b.ID, "zips")
				if err := os.MkdirAll(zipsDir, 0750); err != nil {
					return false, fmt.Errorf("creating zips dir: %w", err)
				}
				dest := filepath.Join(zipsDir, f.Name)
				if !fileExists(dest, f.Size) {
					o.ensureDiskHeadroom(ctx, zipsDir, f.Size)
					slog.InfoContext(ctx, "Downloading part for import", "part", f.Name, "index", partNum, "total", len(b.Files))
					if err := o.driveClient.DownloadFile(ctx, f.ID, dest, f.MD5Checksum, f.Size); err != nil {
						if o.metrics != nil {
							o.metrics.RecordBatchFailed(b.ID, time.Since(batchStartTime))
						}
						return false, fmt.Errorf("downloading part %s: %w", f.Name, err)
					}
					if o.metrics != nil {
						o.metrics.FilesDownloaded.Inc()
						if usage, uErr := getDiskUsageFraction(o.cfg.ScratchDir); uErr == nil {
							o.metrics.DiskUsageRatio.Set(usage)
						}
					}
				}

				slog.InfoContext(ctx, "Importing part into Immich", "file", f.Name, "part", partNum, "total", len(b.Files))
				stats, err := o.immichRun.ImportBatch(ctx, metadataDir, []string{dest})
				if err != nil {
					if o.metrics != nil {
						o.metrics.RecordBatchFailed(b.ID, time.Since(batchStartTime))
					}
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
				return true, nil
			}, dbos.WithStepName(stepName), dbos.WithStepMaxRetries(2))
			if err != nil {
				_ = o.recordBatchStatus(wCtx, b, "FAILED", err.Error(), "RecordFailed-"+stepName)
				return "", fmt.Errorf("importing part %s (%d/%d): %w", f.Name, partNum, len(b.Files), err)
			}
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
					if o.metrics != nil {
						o.metrics.RecordBatchFailed(b.ID, time.Since(batchStartTime))
					}
					return false, fmt.Errorf("trashing file %s (%s): %w", f.Name, f.ID, err)
				}
			}
			return true, nil
		}, dbos.WithStepName("TrashBatch-"+b.ID), dbos.WithStepMaxRetries(3))
		if err != nil {
			_ = o.recordBatchStatus(wCtx, b, "FAILED", err.Error(), "RecordFailed-TrashBatch-"+b.ID)
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

		// Phase 5: Purge durable metadata slices and bundle from database once batch has finished
		if err := o.deleteMetadataSlices(wCtx, b.ID, "DeleteMetadataSlices-"+b.ID); err != nil {
			slog.Warn("Failed to delete metadata slices from database", "batchID", b.ID, "error", err)
		}
		if err := o.deleteMetadataBundle(wCtx, b.ID, "DeleteMetadataBundle-"+b.ID); err != nil {
			slog.Warn("Failed to delete metadata bundle from database", "batchID", b.ID, "error", err)
		}

		// Phase 6: Mark batch completed via DBOS transaction (Recommendation 2.3)
		if err := o.recordBatchStatus(wCtx, b, "COMPLETED", "", "MarkCompleted-"+b.ID); err != nil {
			return "", err
		}

		// Record batch completion metrics inside a step (Recommendation 2.2)
		_, err = dbos.RunAsStep(wCtx, func(ctx context.Context) (bool, error) {
			if o.metrics != nil {
				o.metrics.RecordBatchCompleted(b.ID, time.Since(batchStartTime))
			}
			return true, nil
		}, dbos.WithStepName("RecordCompletedMetrics-"+b.ID))
		if err != nil {
			slog.Warn("Recording completed metrics step encountered error", "batchID", b.ID, "error", err)
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

func (o *Orchestrator) recordBatchStatus(wCtx dbos.Context, b drive.TakeoutBatch, status string, errMsg string, stepName string) error {
	_, err := dbos.RunAsTransaction(wCtx, o.ds, func(ctx context.Context, tx dbos.Tx) (bool, error) {
		return true, o.recordBatchStatusTx(ctx, tx, b, status, errMsg)
	}, dbos.WithStepName(stepName))
	return err
}

func (o *Orchestrator) recordBatchStatusTx(ctx context.Context, tx dbos.Tx, b drive.TakeoutBatch, status string, errMsg string) error {
	query := `
	INSERT INTO export_batches (batch_id, export_date, status, part_count, total_bytes, error_message, updated_at)
	VALUES ($1, $2, $3, $4, $5, $6, CURRENT_TIMESTAMP)
	ON CONFLICT(batch_id) DO UPDATE SET
		status = excluded.status,
		error_message = excluded.error_message,
		updated_at = CURRENT_TIMESTAMP;
	`
	_, err := tx.Exec(ctx, query, b.ID, b.ExportDate, status, len(b.Files), b.TotalBytes, errMsg)
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
		_, err = tx.Exec(ctx, fileQuery, f.ID, b.ID, f.Name, f.Size, f.MD5Checksum, isDownloaded, isTrashed)
		if err != nil {
			return fmt.Errorf("updating batch_files: %w", err)
		}
	}
	return nil
}

func (o *Orchestrator) saveMetadataSlice(wCtx dbos.Context, batchID string, fileID string, sliceData []byte, stepName string) error {
	_, err := dbos.RunAsTransaction(wCtx, o.ds, func(ctx context.Context, tx dbos.Tx) (bool, error) {
		query := `
		INSERT INTO batch_metadata_slices (batch_id, file_id, slice_data, created_at)
		VALUES ($1, $2, $3, CURRENT_TIMESTAMP)
		ON CONFLICT(batch_id, file_id) DO UPDATE SET
			slice_data = excluded.slice_data,
			created_at = CURRENT_TIMESTAMP;
		`
		if _, err := tx.Exec(ctx, query, batchID, fileID, sliceData); err != nil {
			return false, fmt.Errorf("saving metadata slice: %w", err)
		}
		return true, nil
	}, dbos.WithStepName(stepName))
	return err
}

func (o *Orchestrator) deleteMetadataSlices(wCtx dbos.Context, batchID string, stepName string) error {
	_, err := dbos.RunAsTransaction(wCtx, o.ds, func(ctx context.Context, tx dbos.Tx) (bool, error) {
		if _, err := tx.Exec(ctx, "DELETE FROM batch_metadata_slices WHERE batch_id = $1", batchID); err != nil {
			return false, fmt.Errorf("deleting metadata slices: %w", err)
		}
		return true, nil
	}, dbos.WithStepName(stepName))
	return err
}

func (o *Orchestrator) assembleMetadataSlices(ctx context.Context, batchID string, metaDir string) (int64, int, error) {
	if err := os.MkdirAll(metaDir, 0750); err != nil {
		return 0, 0, fmt.Errorf("creating metadata dir %s: %w", metaDir, err)
	}

	rows, err := o.db.QueryContext(ctx, "SELECT slice_data FROM batch_metadata_slices WHERE batch_id = $1", batchID)
	if err != nil {
		return 0, 0, fmt.Errorf("querying metadata slices for batch %s: %w", batchID, err)
	}
	defer rows.Close()

	totalSlices := 0
	for rows.Next() {
		var sliceData []byte
		if err := rows.Scan(&sliceData); err != nil {
			return 0, 0, fmt.Errorf("scanning metadata slice: %w", err)
		}
		if err := extractTarGz(sliceData, metaDir); err != nil {
			return 0, 0, fmt.Errorf("extracting metadata slice to disk: %w", err)
		}
		totalSlices++
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterating metadata slices: %w", err)
	}

	var totalBytes int64
	var fileCount int
	err = filepath.Walk(metaDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			totalBytes += info.Size()
			fileCount++
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("calculating metadata directory size: %w", err)
	}

	return totalBytes, fileCount, nil
}

func (o *Orchestrator) saveMetadataBundle(wCtx dbos.Context, batchID string, bundleData []byte, stepName string) error {
	_, err := dbos.RunAsTransaction(wCtx, o.ds, func(ctx context.Context, tx dbos.Tx) (bool, error) {
		query := `
		INSERT INTO batch_metadata_bundles (batch_id, bundle_data, created_at)
		VALUES ($1, $2, CURRENT_TIMESTAMP)
		ON CONFLICT(batch_id) DO UPDATE SET
			bundle_data = excluded.bundle_data,
			created_at = CURRENT_TIMESTAMP;
		`
		if _, err := tx.Exec(ctx, query, batchID, bundleData); err != nil {
			return false, fmt.Errorf("saving metadata bundle: %w", err)
		}
		return true, nil
	}, dbos.WithStepName(stepName))
	return err
}

func (o *Orchestrator) deleteMetadataBundle(wCtx dbos.Context, batchID string, stepName string) error {
	_, err := dbos.RunAsTransaction(wCtx, o.ds, func(ctx context.Context, tx dbos.Tx) (bool, error) {
		if _, err := tx.Exec(ctx, "DELETE FROM batch_metadata_bundles WHERE batch_id = $1", batchID); err != nil {
			return false, fmt.Errorf("deleting metadata bundle: %w", err)
		}
		return true, nil
	}, dbos.WithStepName(stepName))
	return err
}

func (o *Orchestrator) ensureMetadataOnDisk(ctx context.Context, batchID string, metaDir string) error {
	entries, err := os.ReadDir(metaDir)
	if err == nil && len(entries) > 0 {
		return nil
	}

	slog.InfoContext(ctx, "Restoring metadata from database slices to local disk", "batchID", batchID, "metaDir", metaDir)
	_, _, err = o.assembleMetadataSlices(ctx, batchID, metaDir)
	return err
}

func createTarGz(sourceDir string) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	err := filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relPath)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		// #nosec G304 -- source path is within validated sourceDir
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(tw, file)
		return err
	})
	if err != nil {
		_ = tw.Close()
		_ = gw.Close()
		return nil, err
	}
	if err := tw.Close(); err != nil {
		_ = gw.Close()
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func extractTarGz(data []byte, destDir string) error {
	if err := os.MkdirAll(destDir, 0750); err != nil {
		return err
	}
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		cleanName := filepath.Clean(header.Name)
		target := filepath.Join(destDir, cleanName)
		cleanDest := filepath.Clean(destDir)
		if !strings.HasPrefix(target, cleanDest+string(filepath.Separator)) && target != cleanDest {
			return fmt.Errorf("illegal file path in tar archive: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0750); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
				return err
			}
			// #nosec G304 -- destination path validated above
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, header.FileInfo().Mode())
			if err != nil {
				return err
			}
			// #nosec G110 -- reading sidecar JSONs from trusted internal bundle
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
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

// extractJSONSidecarsToTarGz reads all .json files from the zip and packs them directly into an in-memory .tar.gz bundle.
func extractJSONSidecarsToTarGz(zipPath string) ([]byte, int, error) {
	cleanZip := filepath.Clean(zipPath)
	r, err := zip.OpenReader(cleanZip)
	if err != nil {
		return nil, 0, fmt.Errorf("opening zip file %s: %w", cleanZip, err)
	}
	defer func() {
		_ = r.Close()
	}()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	count := 0
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(f.Name), ".json") {
			continue
		}
		cleanName := filepath.Clean(f.Name)
		if strings.HasPrefix(cleanName, "..") || filepath.IsAbs(cleanName) {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			_ = tw.Close()
			_ = gw.Close()
			return nil, 0, fmt.Errorf("reading file %s in zip: %w", f.Name, err)
		}

		header := &tar.Header{
			Name:    filepath.ToSlash(cleanName),
			Mode:    0600,
			Size:    f.FileInfo().Size(),
			ModTime: f.Modified,
		}
		if err := tw.WriteHeader(header); err != nil {
			_ = rc.Close()
			_ = tw.Close()
			_ = gw.Close()
			return nil, 0, fmt.Errorf("writing tar header for %s: %w", cleanName, err)
		}

		const maxJSONBytes = 50 * 1024 * 1024
		// #nosec G110 -- limit reader
		_, cpErr := io.Copy(tw, io.LimitReader(rc, maxJSONBytes))
		_ = rc.Close()
		if cpErr != nil {
			_ = tw.Close()
			_ = gw.Close()
			return nil, 0, fmt.Errorf("copying json content from %s: %w", cleanName, cpErr)
		}
		count++
	}

	if err := tw.Close(); err != nil {
		_ = gw.Close()
		return nil, 0, err
	}
	if err := gw.Close(); err != nil {
		return nil, 0, err
	}

	return buf.Bytes(), count, nil
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
