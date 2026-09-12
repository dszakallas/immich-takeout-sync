# Agent instructions for immich-takeout-sync

## Required Go skills

- `golang-cli`: CLI command patterns, signal handling, and exit code conventions.
- `golang-spf13-cobra`: Cobra command tree structure and flag binding.
- `golang-code-style`: Go code clarity, conventions, and formatting.
- `golang-error-handling`: Idiomatic Go error handling and wrapping with `%w`.
- `golang-testing`: Table-driven tests with named subtests and race detection.
- `golang-lint`: Static analysis and `golangci-lint` configuration.

## Development workflow

### Commands

- Build binary:
  ```bash
  go build -ldflags="-s -w -X github.com/dszakallas/immich-takeout-sync/cmd.Version=0.1.0" -o bin/takeout-sync .
  ```
- Run tests:
  ```bash
  go test -v -race ./...
  ```
- Run linter:
  ```bash
  golangci-lint run ./...
  ```

### Pre-commit checks

Ensure all commits pass:
1. `go test -race ./...`
2. `golangci-lint run ./...`
3. `gofmt -s -w .`
