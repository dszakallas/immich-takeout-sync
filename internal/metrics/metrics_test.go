package metrics

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMetricsAndServer(t *testing.T) {
	t.Parallel()

	m := New()
	srv, err := StartServer("127.0.0.1:0", m)
	if err != nil {
		t.Fatalf("StartServer failed: %v", err)
	}
	defer func() {
		_ = srv.Stop(context.Background())
	}()

	m.ImportedAssetsTotal.WithLabelValues("batch-1", "success").Add(42)
	m.ImportedAssetsTotal.WithLabelValues("batch-1", "error").Add(3)
	m.AssetErrorsTotal.WithLabelValues("batch-1", "server_error").Add(3)
	m.DiskUsageRatio.Set(0.45)
	m.FilesDownloaded.Inc()
	m.BytesDownloadedTotal.Add(1024)
	m.RecordBatchCompleted("batch-1", 10*time.Second)

	addr := srv.Addr()
	res, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("failed to GET /metrics: %v", err)
	}
	defer func() {
		_ = res.Body.Close()
	}()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", res.StatusCode)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	bodyStr := string(body)
	expectedSubstrings := []string{
		`takeout_sync_assets_imported_total{batch_id="batch-1",status="success"} 42`,
		`takeout_sync_assets_imported_total{batch_id="batch-1",status="error"} 3`,
		`takeout_sync_assets_errors_total{batch_id="batch-1",reason="server_error"} 3`,
		`takeout_sync_disk_usage_ratio 0.45`,
		`takeout_sync_drive_files_downloaded_total 1`,
		`takeout_sync_drive_downloaded_bytes_total 1024`,
		`takeout_sync_batch_processed_total{status="completed"} 1`,
	}

	for _, sub := range expectedSubstrings {
		if !strings.Contains(bodyStr, sub) {
			t.Errorf("metrics output missing expected substring %q", sub)
		}
	}
}
