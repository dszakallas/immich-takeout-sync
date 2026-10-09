# Google Photos Takeout to Immich Sync Specification

## Overview

`immich-takeout-sync` is a durable, fault-tolerant synchronization service that automates the ingestion of Google Photos Takeout archives from Google Drive into an Immich instance.

Google Takeout exports can be scheduled periodically (e.g. bi-monthly for a year), producing either initial full snapshots (spanning tens to hundreds of gigabytes across many split archives) or incremental batches containing newly captured or modified assets. This service processes these multi-part archives reliably under strict scratch disk constraints, extracts metadata sidecars across parts before asset ingestion, handles intermittent network or asset-level errors gracefully, and exposes Prometheus metrics for observability.

## Architecture

The system consists of five primary components coordinated through a durable workflow:

1. **Drive client (`internal/drive`)**: Queries the Google Drive API, discovers archive files, handles OAuth2 token lifecycle with automatic refresh, and streams archives using resumable HTTP `Range` requests.
2. **Workflow orchestrator (`internal/workflow`)**: Powered by the DBOS Go SDK, executes deterministic multi-step workflows with durable state persistence across supported database backends (e.g. SQLite, PostgreSQL).
3. **Immich runner (`internal/immich`)**: Wraps `immich-go` execution, manages CLI parameters, captures logs, and parses asset-level upload statistics.
4. **Metrics engine (`internal/metrics`)**: Exposes Prometheus metrics on `/metrics` via an HTTP server, tracking asset imports, error classifications, downloaded bytes, disk usage, and batch runtimes.
5. **CLI & configuration (`cmd`, `internal/config`)**: Provides Cobra commands, flag bindings, and environment variable configuration.

```
+-------------------------------------------------------------------------+
|                              Google Drive                               |
+------------------------------------+------------------------------------+
                                     |
                         Resumable HTTP Range GET
                                     v
+-------------------------------------------------------------------------+
|                           immich-takeout-sync                           |
|                                                                         |
|  +-------------------------------------------------------------------+  |
|  |                    DBOS Workflow Orchestrator                     |  |
|  |                                                                   |  |
|  |  Step 1: Discover & Register Batches                              |  |
|  |  Step 2: Phase 1 - Incremental Metadata Slice Extraction & DBOS Tx|  |
|  |  Step 3: Assemble Consolidated Metadata Directory                 |  |
|  |  Step 4: Phase 2 - Granular Per-Part Ingestion with Lazy Restore   |  |
|  |  Step 5: Drive Lifecycle Management (Optional Trash/Delete)       |  |
|  |  Step 6: Local Scratch Storage Purge                              |  |
|  |  Step 7: Purge Metadata Slices & Record Completion Transaction     |  |
|  +-----------------------------------+-------------------------------+  |
|                                      |                                  |
|         +----------------------------+----------------------------+     |
|         |                            |                            |     |
|         v                            v                            v     |
|  +--------------+            +---------------+            +----------+  |
|  | Local Scratch|            | Prometheus    |            | immich-go|  |
|  | Storage      |            | /metrics      |            | Runner   |  |
|  +--------------+            +---------------+            +----+-----+  |
+----------------------------------------------------------------|--------+
                                                                 |
                                                          Upload Assets &
                                                          Sidecar Metadata
                                                                 v
                                                       +------------------+
                                                       |  Immich Server   |
                                                       +------------------+
```

## Takeout Batch Discovery and Grouping

Google Takeout creates archive files following specific naming conventions:

- Snapshot split archive: `takeout-<YYYYMMDD>T<HHMMSS>Z-<part_group>-<part_index>.zip` (e.g. `takeout-20260711T074714Z-2-001.zip`).
- Incremental archive: `takeout-<YYYYMMDD>T<HHMMSS>Z-<part_index>.zip` or single-file exports without part groups.

### Batch identification

Files sharing the same ISO 8601 timestamp prefix belong to the same export batch (`BatchID`).

- A batch is classified as an **increment** if it consists of small individual archives without intermediate part groups.
- A batch is classified as a **full snapshot** if it spans multiple multi-gigabyte split parts.
- Batches are ordered chronologically by timestamp so that initial snapshots are ingested prior to subsequent increments.

## Storage and Disk-Bounded Ingestion

Large Takeout archives (e.g. 100+ GB) can easily exceed local scratch volume capacity. The service enforces a bounded two-phase processing model.

### Phase 1: Incremental metadata pre-extraction pass

Google Photos Takeout splits photos and their associated JSON sidecars arbitrarily across archive parts. A photo in `part-005.zip` may have its `.supplemental-metadata.json` sidecar located in `part-001.zip`.

1. The service iterates through each archive file in the batch as an independent DBOS step (`ExtractMetadataSlice-<batchID>-<partNum>`).
2. If the zip is not present locally, it is streamed from Google Drive into `<scratch>/<batchID>/zips/<filename>`.
3. The zip archive is opened without full decompression, and all `*.json` files (metadata sidecars and album metadata) are extracted directly into an in-memory `.tar.gz` metadata slice.
4. If disk space exceeds a configurable high watermark (default: 90% utilization), already-extracted zips are evicted from the local cache to free space for remaining parts.
5. Each metadata slice is checkpointed individually into the database in the `batch_metadata_slices` table via a DBOS transaction (`SaveMetadataSlice-<batchID>-<partNum>`). If extraction is interrupted midway through a multi-part batch, already-extracted parts are preserved and skipped upon restart.
6. Once all slices are checkpointed, the workflow executes `AssembleMetadata-<batchID>`, which unpacks all slices into `<scratch>/<batchID>/metadata/` on disk and calculates the exact consolidated metadata size.

### Phase 2: Granular per-part sequential import pass

Once all JSON sidecars for the entire batch are extracted and durably persisted, asset import executes as granular per-part DBOS steps (`ImportPart-<batchID>-<partNum>`):

1. The workflow iterates sequentially through each archive part (`part-001`, `part-002`, ..., `part-NNN`), executing each part as an independent DBOS step.
2. **Resumption & metadata availability guarantee**: At the start of each part step, the workflow checks if the local metadata directory exists and contains files. If the process was restarted in a new container or the scratch disk was cleared, the step lazily restores and extracts the metadata slices from `batch_metadata_slices` before proceeding.
3. At most **one** zip archive is maintained on disk for ingestion. If the zip was evicted during Phase 1, it is re-downloaded.
4. `immich-go` is executed pointing to both the metadata directory and the single archive part:
   ```bash
   immich-go upload from-google-photos \
     --server="http://immich:2283" \
     --api-key="***" \
     --no-ui=true \
     --on-errors=continue \
     --sync-albums=true \
     --takeout-tag=true \
     --people-tag=true \
     --include-archived=true \
     "<scratch>/<batchID>/metadata" \
     "<scratch>/<batchID>/zips/<part>.zip"
   ```
5. `immich-go` reads sidecars from the metadata directory to properly assign album associations, descriptions, geolocation, and timestamps to the assets inside the current zip part.
6. Immediately after the part finishes ingestion, the zip file is removed from disk to reclaim storage space before proceeding to the next part.
7. Once all parts and drive lifecycle steps succeed, local scratch files are purged (`CleanupScratch-<batchID>`) and durable metadata slices are deleted from `batch_metadata_slices` (`DeleteMetadataSlices-<batchID>`) via DBOS transaction, leaving zero residual storage footprint.

## Resumable Downloads

To handle network interruptions and multi-gigabyte files reliably:

- Downloads inspect existing partial files (`.downloading` extension) and retrieve the current size.
- The HTTP request sets `Range: bytes=<offset>-` to resume from the last byte.
- If the server returns HTTP 206 (Partial Content), streaming appends to the existing file.
- If the server returns HTTP 200, the partial file is truncated and restarted from offset 0.
- Checksums: To verify MD5 integrity on resumed downloads without re-downloading, existing file bytes are streamed through the MD5 hasher before appending newly received bytes.
- Progress tracking resets consecutive failure counts to zero whenever forward byte progress is made during an attempt, preventing retry exhaustion on slow or flaky links.

## DBOS Workflow & Transaction Semantics

The synchronization pipeline executes inside durable DBOS workflows (`SyncPipelineWorkflow`) governed by the following design principles:

1. **Transactional Mutations**: All application database mutations (`export_batches`, `batch_files`, `batch_metadata_slices`, `batch_metadata_bundles`) execute inside `dbos.RunAsTransaction` blocks rather than raw `dbos.RunAsStep` operations, guaranteeing atomicity and automatic rollback upon error.
2. **Metrics Emission Inside Steps**: Prometheus metrics (counters, gauges, histograms) are recorded exclusively inside step execution bodies or dedicated transaction steps, preventing duplicate or ghost metric increments when steps are replayed during workflow recovery.
3. **Deterministic Timers**: Batch timing starts with a checkpointed `BatchStart-<batchID>` step that durably records `time.Now()`, ensuring that resumed workflows calculate execution durations deterministically.
4. **Crash-Resilience & Re-entrant Execution**: In the event of process restart, container eviction, or network interruptions:
   - Previously completed metadata slices and imported parts are recognized as completed in DBOS and skipped.
   - If the scratch disk was cleared, `ensureMetadataOnDisk` reconstructs the required metadata directory from durable `batch_metadata_slices` before invoking `immich-go`.
   - Partial file downloads automatically resume from their last byte position.

## Fault-Tolerant Asset Error Handling

When ingesting archives containing corrupted images, unsupported formats, or duplicate stack collisions, `immich-go` records the failure and proceeds when `--on-errors=continue` is active. However, `immich-go` terminates with exit code 1 whenever any asset errors occur.

The runner discriminates between non-fatal asset errors and fatal process failures:

1. **Log & report parsing**: The runner captures process output and inspects the `Asset Tracking Report` generated by `immich-go`.
2. **Partial error classification**: If `immich-go` completed its discovery and upload phases, processed assets, or reported specific asset failures (e.g. `ERR server error`, `upload failed`), the run is treated as successful.
3. **Fatal error detection**: If `immich-go` exited non-zero without generating an asset report (due to invalid credentials, unreachable server, or crash), the workflow fails the step and triggers DBOS retry policies.

## Prometheus Metrics

The service exposes metrics at `:9090/metrics` (configurable via `--metrics-addr`):

| Metric Name | Type | Labels | Description |
|---|---|---|---|
| `takeout_sync_assets_imported_total` | Counter | `batch_id`, `status` (`success`, `upgraded`, `discarded`, `error`) | Total assets processed by `immich-go`. |
| `takeout_sync_assets_errors_total` | Counter | `batch_id`, `reason` | Count of asset-level import errors by failure reason. |
| `takeout_sync_drive_files_downloaded_total` | Counter | None | Total archive files downloaded from Google Drive. |
| `takeout_sync_drive_downloaded_bytes_total` | Counter | None | Total bytes downloaded from Google Drive. |
| `takeout_sync_disk_usage_ratio` | Gauge | None | Current filesystem usage ratio (0.0 to 1.0) of scratch storage. |
| `takeout_sync_batch_duration_seconds` | Histogram | `batch_id`, `status` (`completed`, `failed`) | Duration of entire batch synchronization. |
| `takeout_sync_batch_processed_total` | Counter | `status` (`completed`, `failed`) | Total number of batches processed. |

## Database Schema

The service uses a relational database supported by DBOS (e.g. SQLite, PostgreSQL) for state tracking and DBOS workflow history:

### `export_batches`

Tracks batches identified from Google Drive:

```sql
CREATE TABLE export_batches (
    batch_id TEXT PRIMARY KEY,
    export_date TIMESTAMP NOT NULL,
    status TEXT NOT NULL,          -- PENDING, PROCESSING, COMPLETED, FAILED
    part_count INTEGER NOT NULL,
    total_bytes BIGINT NOT NULL,
    error_message TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

### `batch_files`

Tracks individual archive parts within each batch:

```sql
CREATE TABLE batch_files (
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
```

### `batch_metadata_bundles`

Temporarily holds compressed `.tar.gz` metadata bundles for active batches, guaranteeing metadata sidecar availability during resumed part executions regardless of local scratch disk eviction:

```sql
CREATE TABLE batch_metadata_bundles (
    batch_id TEXT PRIMARY KEY REFERENCES export_batches(batch_id),
    bundle_data BYTEA NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

### `batch_metadata_slices`

Temporarily holds compressed `.tar.gz` metadata slices checkpointed per archive part, guaranteeing incremental resumption during extraction and metadata sidecar availability during resumed part executions:

```sql
CREATE TABLE batch_metadata_slices (
    batch_id TEXT NOT NULL,
    file_id TEXT NOT NULL,
    slice_data BYTEA NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (batch_id, file_id)
);
```

## Google Drive Lifecycle

- By default, `--delete-from-drive` is set to `false`. Google Drive archives are preserved untouched.
- When explicitly enabled (`--delete-from-drive=true`), completed archives are moved to the Google Drive trash only after the entire batch has been successfully imported and verified.
- The `--dry-run` flag allows previewing discovery, extraction, and sync steps without downloading or uploading assets.
