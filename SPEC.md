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
|  |  Step 2: Phase 1 - Download & Extract Metadata Across Parts      |  |
|  |  Step 3: Phase 2 - Sequential Import & Immediate Eviction         |  |
|  |  Step 4: Drive Lifecycle Management (Optional Trash/Delete)       |  |
|  |  Step 5: Scratch Purge & Completion Recording                     |  |
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

### Phase 1: Metadata pre-extraction pass

Google Photos Takeout splits photos and their associated JSON sidecars arbitrarily across archive parts. A photo in `part-005.zip` may have its `.supplemental-metadata.json` sidecar located in `part-001.zip`.

1. The service iterates through each archive file in the batch.
2. If the zip is not present locally, it is streamed from Google Drive into `<scratch>/<batchID>/zips/<filename>`.
3. The zip archive is opened without full decompression, and all `*.json` files (metadata sidecars and album metadata) are extracted into `<scratch>/<batchID>/metadata/Takeout/Google Photos/`.
4. If disk space exceeds a configurable high watermark (default: 90% utilization), already-extracted zips are evicted from the local cache to free space for remaining parts.

### Phase 2: Sequential import pass

Once all JSON sidecars for the entire batch are extracted into the shared metadata directory, asset import begins:

1. The workflow iterates sequentially through each archive part (`part-001`, `part-002`, ..., `part-NNN`).
2. At most **one** zip archive is maintained on disk for ingestion. If the zip was evicted during Phase 1, it is re-downloaded.
3. `immich-go` is executed pointing to both the shared metadata directory and the single archive part:
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
4. `immich-go` reads sidecars from the shared metadata directory to properly assign album associations, descriptions, geolocation, and timestamps to the assets inside the current zip part.
5. Immediately after the part finishes ingestion, the zip file is removed from disk to reclaim storage space before proceeding to the next part.

## Resumable Downloads

To handle network interruptions and multi-gigabyte files reliably:

- Downloads inspect existing partial files (`.tmp` extension) and retrieve the current size.
- The HTTP request sets `Range: bytes=<offset>-` to resume from the last byte.
- If the server returns HTTP 206 (Partial Content), streaming appends to the existing file.
- If the server returns HTTP 200, the partial file is truncated and restarted from offset 0.
- Checksums: To verify MD5 integrity on resumed downloads without re-downloading, existing file bytes are streamed through the MD5 hasher before appending newly received bytes.
- Progress tracking resets consecutive failure counts to zero whenever forward byte progress is made during an attempt, preventing retry exhaustion on slow or flaky links.

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
    file_id TEXT PRIMARY KEY,
    batch_id TEXT NOT NULL REFERENCES export_batches(batch_id),
    filename TEXT NOT NULL,
    part_number INTEGER NOT NULL,
    size_bytes BIGINT NOT NULL,
    md5_checksum TEXT,
    status TEXT NOT NULL,          -- PENDING, DOWNLOADED, EXTRACTED, IMPORTED, DELETED
    drive_trashed BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

## Google Drive Lifecycle

- By default, `--delete-from-drive` is set to `false`. Google Drive archives are preserved untouched.
- When explicitly enabled (`--delete-from-drive=true`), completed archives are moved to the Google Drive trash only after the entire batch has been successfully imported and verified.
- The `--dry-run` flag allows previewing discovery, extraction, and sync steps without downloading or uploading assets.
