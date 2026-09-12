package cmd

import (
	"fmt"
	"log/slog"

	"github.com/dszakallas/immich-takeout-sync/internal/config"
	"github.com/dszakallas/immich-takeout-sync/internal/drive"
	"github.com/dszakallas/immich-takeout-sync/internal/immich"
	"github.com/dszakallas/immich-takeout-sync/internal/metrics"
	"github.com/dszakallas/immich-takeout-sync/internal/workflow"
	"github.com/spf13/cobra"
)

var (
	deleteFromDrive bool
	highWatermark   float64
	scratchDir      string
	dryRun          bool
	folderID        string
	metricsAddr     string
)

func init() {
	syncCmd.Flags().BoolVar(&deleteFromDrive, "delete-from-drive", false, "Soft-delete (trash) archives from Google Drive after import (default false)")
	syncCmd.Flags().Float64Var(&highWatermark, "high-watermark", 0.90, "Disk usage high watermark ratio before evicting cached zips")
	syncCmd.Flags().StringVar(&scratchDir, "scratch-dir", "", "Scratch directory for staging downloads")
	syncCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Simulate actions without changes to Immich or Drive")
	syncCmd.Flags().StringVar(&folderID, "folder-id", "", "Google Drive folder ID containing Takeout archives")
	syncCmd.Flags().StringVar(&metricsAddr, "metrics-addr", ":9090", "Address for Prometheus metrics HTTP server (set to 'none' to disable)")
}

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Run the Takeout to Immich synchronization workflow",
	Long: `Discovers Takeout archives in Google Drive, extracts metadata sidecars,
sequentially downloads and ingests parts into Immich via immich-go,
optionally soft-deletes the files in Google Drive, and maintains durable checkpoints via DBOS.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.LoadFromEnv()
		if err != nil {
			return fmt.Errorf("loading configuration: %w", err)
		}

		if cmd.Flags().Changed("delete-from-drive") {
			cfg.DeleteFromDrive = deleteFromDrive
		}
		if cmd.Flags().Changed("high-watermark") {
			cfg.DiskHighWatermark = highWatermark
		}
		if cmd.Flags().Changed("scratch-dir") {
			cfg.ScratchDir = scratchDir
		}
		if cmd.Flags().Changed("dry-run") {
			cfg.DryRun = dryRun
		}
		if cmd.Flags().Changed("folder-id") {
			cfg.GoogleDriveFolderID = folderID
		}
		if cmd.Flags().Changed("metrics-addr") {
			cfg.MetricsAddr = metricsAddr
		}

		ctx := cmd.Context()

		driveClient, err := drive.NewClient(ctx, cfg)
		if err != nil {
			return fmt.Errorf("initializing Google Drive client: %w", err)
		}

		immichRunner := immich.NewRunner(cfg)

		metricInstance := metrics.New()
		driveClient.SetMetrics(metricInstance)
		metricsSrv, err := metrics.StartServer(cfg.MetricsAddr, metricInstance)
		if err != nil {
			slog.WarnContext(ctx, "Failed to start metrics server", "addr", cfg.MetricsAddr, "error", err)
		} else if metricsSrv != nil {
			defer func() {
				_ = metricsSrv.Stop(ctx)
			}()
		}

		orchestrator, err := workflow.NewOrchestrator(ctx, cfg, driveClient, immichRunner, metricInstance)
		if err != nil {
			return fmt.Errorf("initializing workflow orchestrator: %w", err)
		}
		defer func() {
			_ = orchestrator.Close()
		}()

		return orchestrator.Start(ctx)
	},
}
