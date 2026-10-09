package workflow

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestTarGzRoundTrip(t *testing.T) {
	tempSrc := t.TempDir()
	tempDst := t.TempDir()

	// Create sample directory structure with JSON sidecars
	subDir := filepath.Join(tempSrc, "Takeout", "Google Photos", "Album1")
	if err := os.MkdirAll(subDir, 0750); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	file1 := filepath.Join(tempSrc, "root.json")
	if err := os.WriteFile(file1, []byte(`{"title": "root"}`), 0600); err != nil {
		t.Fatalf("failed to write file1: %v", err)
	}

	file2 := filepath.Join(subDir, "photo.jpg.json")
	if err := os.WriteFile(file2, []byte(`{"title": "photo.jpg", "description": "beach"}`), 0600); err != nil {
		t.Fatalf("failed to write file2: %v", err)
	}

	// Pack
	data, err := createTarGz(tempSrc)
	if err != nil {
		t.Fatalf("createTarGz failed: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty tar.gz byte slice")
	}

	// Unpack
	if err := extractTarGz(data, tempDst); err != nil {
		t.Fatalf("extractTarGz failed: %v", err)
	}

	// Verify files restored
	restoredFile1 := filepath.Join(tempDst, "root.json")
	content1, err := os.ReadFile(restoredFile1)
	if err != nil {
		t.Fatalf("failed to read restored file1: %v", err)
	}
	if string(content1) != `{"title": "root"}` {
		t.Fatalf("unexpected content for file1: %s", string(content1))
	}

	restoredFile2 := filepath.Join(tempDst, "Takeout", "Google Photos", "Album1", "photo.jpg.json")
	content2, err := os.ReadFile(restoredFile2)
	if err != nil {
		t.Fatalf("failed to read restored file2: %v", err)
	}
	if string(content2) != `{"title": "photo.jpg", "description": "beach"}` {
		t.Fatalf("unexpected content for file2: %s", string(content2))
	}
}

func TestExtractTarGzPathTraversal(t *testing.T) {
	tempDst := t.TempDir()

	// Malicious tar containing ../escaped.txt
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	maliciousHeader := &tar.Header{
		Name: "../escaped.txt",
		Mode: 0600,
		Size: int64(len("malicious")),
	}
	if err := tw.WriteHeader(maliciousHeader); err != nil {
		t.Fatalf("failed to write header: %v", err)
	}
	if _, err := tw.Write([]byte("malicious")); err != nil {
		t.Fatalf("failed to write content: %v", err)
	}
	_ = tw.Close()
	_ = gw.Close()

	err := extractTarGz(buf.Bytes(), tempDst)
	if err == nil {
		t.Fatal("expected error on path traversal in tar archive, got nil")
	}
}

func TestInitSchemaWithMetadataBundles(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite in-memory: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := initSchema(ctx, db); err != nil {
		t.Fatalf("initSchema failed: %v", err)
	}

	// Verify tables exist
	tables := []string{"export_batches", "batch_files", "batch_metadata_bundles", "batch_metadata_slices"}
	for _, table := range tables {
		var name string
		err := db.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil || name != table {
			t.Fatalf("expected table %s to exist, err: %v", table, err)
		}
	}
}

func TestExtractJSONSidecarsToTarGzAndAssembly(t *testing.T) {
	tempDir := t.TempDir()
	zip1Path := filepath.Join(tempDir, "takeout-1.zip")
	zip2Path := filepath.Join(tempDir, "takeout-2.zip")

	// Helper to create a zip file
	createZip := func(p string, files map[string]string) {
		f, err := os.Create(p)
		if err != nil {
			t.Fatalf("failed to create zip file: %v", err)
		}
		zw := zip.NewWriter(f)
		for name, content := range files {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatalf("failed to create zip entry: %v", err)
			}
			if _, err := w.Write([]byte(content)); err != nil {
				t.Fatalf("failed to write zip content: %v", err)
			}
		}
		_ = zw.Close()
		_ = f.Close()
	}

	createZip(zip1Path, map[string]string{
		"Takeout/Google Photos/photo1.jpg":      "image-binary-data",
		"Takeout/Google Photos/photo1.jpg.json": `{"title":"photo1"}`,
	})
	createZip(zip2Path, map[string]string{
		"Takeout/Google Photos/photo2.jpg":      "image-binary-data-2",
		"Takeout/Google Photos/photo2.jpg.json": `{"title":"photo2"}`,
	})

	slice1, count1, err := extractJSONSidecarsToTarGz(zip1Path)
	if err != nil {
		t.Fatalf("extract slice 1 failed: %v", err)
	}
	if count1 != 1 {
		t.Fatalf("expected count 1, got %d", count1)
	}

	slice2, count2, err := extractJSONSidecarsToTarGz(zip2Path)
	if err != nil {
		t.Fatalf("extract slice 2 failed: %v", err)
	}
	if count2 != 1 {
		t.Fatalf("expected count 1, got %d", count2)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := initSchema(ctx, db); err != nil {
		t.Fatalf("initSchema failed: %v", err)
	}

	batchID := "batch-test-123"
	_, err = db.ExecContext(ctx, "INSERT INTO batch_metadata_slices (batch_id, file_id, slice_data) VALUES (?, ?, ?)", batchID, "f1", slice1)
	if err != nil {
		t.Fatalf("insert slice 1: %v", err)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO batch_metadata_slices (batch_id, file_id, slice_data) VALUES (?, ?, ?)", batchID, "f2", slice2)
	if err != nil {
		t.Fatalf("insert slice 2: %v", err)
	}

	orch := &Orchestrator{db: db}
	metaDir := filepath.Join(tempDir, "assembled_meta")
	totalBytes, fileCount, err := orch.assembleMetadataSlices(ctx, batchID, metaDir)
	if err != nil {
		t.Fatalf("assembleMetadataSlices failed: %v", err)
	}

	if fileCount != 2 {
		t.Fatalf("expected 2 assembled files, got %d", fileCount)
	}
	if totalBytes <= 0 {
		t.Fatalf("expected positive totalBytes, got %d", totalBytes)
	}

	p1 := filepath.Join(metaDir, "Takeout", "Google Photos", "photo1.jpg.json")
	p2 := filepath.Join(metaDir, "Takeout", "Google Photos", "photo2.jpg.json")
	if _, err := os.Stat(p1); err != nil {
		t.Fatalf("file1 not found on disk: %v", err)
	}
	if _, err := os.Stat(p2); err != nil {
		t.Fatalf("file2 not found on disk: %v", err)
	}
}
