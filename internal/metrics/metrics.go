// Package metrics defines and manages Prometheus metrics for takeout-sync.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all Prometheus metrics registered for takeout-sync.
type Metrics struct {
	reg *prometheus.Registry

	ImportedAssetsTotal  *prometheus.CounterVec
	AssetErrorsTotal     *prometheus.CounterVec
	BatchDuration        *prometheus.HistogramVec
	BatchesTotal         *prometheus.CounterVec
	DiskUsageRatio       prometheus.Gauge
	FilesDownloaded      prometheus.Counter
	BytesDownloadedTotal prometheus.Counter
}

// New creates and registers Prometheus metrics for the sync pipeline.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	// Register standard Go runtime and process collectors
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		reg: reg,
		ImportedAssetsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "takeout_sync",
				Subsystem: "assets",
				Name:      "imported_total",
				Help:      "Total number of assets processed by immich-go, partitioned by status (success, upgraded, discarded, error).",
			},
			[]string{"batch_id", "status"},
		),
		AssetErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "takeout_sync",
				Subsystem: "assets",
				Name:      "errors_total",
				Help:      "Total number of asset-level errors during import, partitioned by failure reason.",
			},
			[]string{"batch_id", "reason"},
		),
		BatchDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "takeout_sync",
				Subsystem: "batch",
				Name:      "duration_seconds",
				Help:      "Duration in seconds to process an entire takeout batch.",
				Buckets:   prometheus.ExponentialBuckets(1, 2, 14), // 1s to ~16384s (~4.5h)
			},
			[]string{"batch_id", "status"},
		),
		BatchesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "takeout_sync",
				Subsystem: "batch",
				Name:      "processed_total",
				Help:      "Total number of takeout batches processed, partitioned by final status (completed, failed).",
			},
			[]string{"status"},
		),
		DiskUsageRatio: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: "takeout_sync",
				Subsystem: "disk",
				Name:      "usage_ratio",
				Help:      "Current filesystem usage ratio (0.0 to 1.0) of the scratch volume.",
			},
		),
		FilesDownloaded: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: "takeout_sync",
				Subsystem: "drive",
				Name:      "files_downloaded_total",
				Help:      "Total number of archive files downloaded from Google Drive.",
			},
		),
		BytesDownloadedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: "takeout_sync",
				Subsystem: "drive",
				Name:      "downloaded_bytes_total",
				Help:      "Total bytes downloaded from Google Drive.",
			},
		),
	}

	reg.MustRegister(
		m.ImportedAssetsTotal,
		m.AssetErrorsTotal,
		m.BatchDuration,
		m.BatchesTotal,
		m.DiskUsageRatio,
		m.FilesDownloaded,
		m.BytesDownloadedTotal,
	)

	return m
}

// Handler returns an HTTP handler for scraping Prometheus metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// RecordBatchCompleted marks a batch as completed with its execution duration.
func (m *Metrics) RecordBatchCompleted(batchID string, duration time.Duration) {
	m.BatchesTotal.WithLabelValues("completed").Inc()
	m.BatchDuration.WithLabelValues(batchID, "completed").Observe(duration.Seconds())
}

// RecordBatchFailed marks a batch as failed with its execution duration.
func (m *Metrics) RecordBatchFailed(batchID string, duration time.Duration) {
	m.BatchesTotal.WithLabelValues("failed").Inc()
	m.BatchDuration.WithLabelValues(batchID, "failed").Observe(duration.Seconds())
}
