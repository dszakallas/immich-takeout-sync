# immich-takeout-sync

Automated pipeline for synchronizing Google Photos Takeout archives from Google Drive into an Immich server.

## Features

- **Google Drive polling**: Discovers Takeout exports in a designated Google Drive folder.
- **Chronological batching**: Handles multi-part archives and processes full exports followed by monthly increments in order.
- **Durable checkpointing**: Built with DBOS Transact Go for resumption across restarts.
- **Out-of-process ingestion**: Runs `immich-go upload from-google-photos` for metadata matching, album syncing, and duplicate detection.
- **Soft-delete**: Moves processed archive files in Google Drive to the trash.
- **Bounded scratch storage**: Processes strictly one batch at a time to prevent scratch disk exhaustion during multi-month backfills.
- **Personal Google Drive support**: Supports both Service Account credentials and OAuth2 refresh tokens.

## Commands

### Interactive OAuth2 Login (Personal Google Drive)

If using a personal Google Drive account, create a **Desktop App** OAuth client in Google Cloud Console, then run:

```bash
takeout-sync auth login --client-id="<CLIENT_ID>" --client-secret="<CLIENT_SECRET>" -o google-oauth2-credentials.json
```

This launches a local listener, prompts you to sign in with Google, and writes the refresh token JSON configuration.

### Synchronization

```bash
takeout-sync sync
```

### Environment variables

| Variable | Description | Default |
| :--- | :--- | :--- |
| `GOOGLE_DRIVE_FOLDER_ID` | ID of the Google Drive folder containing Takeout archives | Required |
| `IMMICH_SERVER_URL` | Immich server URL | `http://immich-server.apps.svc:2283` |
| `IMMICH_API_KEY` | Immich API key | Required |
| `DBOS_DATABASE_URL` | Database connection URL supported by DBOS (e.g. SQLite, PostgreSQL) | `sqlite:/data/state.db` |
| `SCRATCH_DIR` | Temporary download storage path | `/scratch` |
| `GOOGLE_CREDENTIALS_JSON` | Service Account JSON or OAuth2 token JSON | - |
| `GOOGLE_APPLICATION_CREDENTIALS` | Path to credentials JSON | - |
| `DRY_RUN` | If `true`, simulates upload and trashing | `false` |
