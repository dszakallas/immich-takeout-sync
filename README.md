# immich-takeout-sync

Automated pipeline for synchronizing Google Photos Takeout archives from Google Drive into an Immich server.

## Features

- **Google Drive**: Discovers Takeout exports in a designated Google Drive folder.
- **Chronological batching**: Handles multi-part archives and processes full exports followed by monthly increments in order.
- **Incremental processing**: Suitable for running on a cron schedule to discover and process new batches as they appear without reprocessing old batches.
- **Granular durable checkpointing**: Built with DBOS Transact Go with per-part step granularity, transactional database updates, and durable metadata sidecar bundling for seamless resumption across process or container restarts & crashes mid-import.
- **Out-of-process ingestion**: Runs `immich-go upload from-google-photos` for metadata matching, album syncing, and duplicate detection.
- **Bounded scratch storage**: Can operate with scratch space that can hold at least the largest part file + metadata (although can result in multiple downloads / part)
- **Soft-delete**: Optionally moves processed archive files in Google Drive to the trash.
- **Personal Google Drive support**: Supports both Service Account credentials and OAuth2 refresh tokens.

## Commands

### Interactive OAuth2 Login (Personal Google Drive)

If using a personal Google Drive account, create a **Desktop App** OAuth client in Google Cloud Console, then run:

```bash
takeout-sync auth login --client-id="<CLIENT_ID>" --client-secret="<CLIENT_SECRET>"
```

This launches a local listener, prompts you to sign in with Google, and writes the refresh token JSON configuration to `$XDG_CONFIG_HOME/immich-takeout-sync/creds.json` (or `~/.config/immich-takeout-sync/creds.json`) by default.
If credentials already exist (in the default location or specified by `-c` / `--credentials-file` or `-o`), `takeout-sync auth login` will automatically reuse the client ID and secret.

### Synchronization

```bash
takeout-sync sync
```

`takeout-sync sync` automatically reads `$XDG_CONFIG_HOME/immich-takeout-sync/creds.json` (or `~/.config/immich-takeout-sync/creds.json`) if it exists.
You can also specify credentials directly on the command line using:
- `--credentials-file` (`-c`): Path to a credentials JSON file or inline JSON string
- `--credentials-json`: Raw JSON credentials string
- `GOOGLE_APPLICATION_CREDENTIALS` / `GOOGLE_CREDENTIALS_JSON` environment variables

### Environment variables

| Variable | Description | Default |
| :--- | :--- | :--- |
| `GOOGLE_DRIVE_FOLDER_ID` | ID of the Google Drive folder containing Takeout archives | Required |
| `IMMICH_SERVER_URL` | Immich server URL | `http://immich-server.apps.svc:2283` |
| `IMMICH_API_KEY` | Immich API key | Required |
| `DBOS_DATABASE_URL` | Database connection URL supported by DBOS (e.g. SQLite, PostgreSQL) | `sqlite:/data/state.db` |
| `SCRATCH_DIR` | Temporary download storage path | `/scratch` |
| `GOOGLE_CREDENTIALS_JSON` | Service Account JSON or OAuth2 token JSON | - |
| `GOOGLE_APPLICATION_CREDENTIALS` | Path to credentials JSON (defaults to `$XDG_CONFIG_HOME/immich-takeout-sync/creds.json` if present) | - |
| `DRY_RUN` | If `true`, simulates upload and trashing | `false` |
