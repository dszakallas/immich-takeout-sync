// Package cmd contains all CLI commands for takeout-sync.
package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"
)

var (
	logLevel  string
	logFormat string
)

var rootCmd = &cobra.Command{
	Use:           "takeout-sync",
	Short:         "Automated Google Photos Takeout backup pipeline to Immich",
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		return setupLogger(cmd.ErrOrStderr(), logLevel, logFormat)
	},
}

// ExecuteContext runs the root command with context.
func ExecuteContext(ctx context.Context) error {
	return rootCmd.ExecuteContext(ctx)
}

func setupLogger(out io.Writer, levelStr string, formatStr string) error {
	var level slog.Level
	switch strings.ToLower(levelStr) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return fmt.Errorf("invalid log level %q: must be debug, info, warn, or error", levelStr)
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}

	var handler slog.Handler
	switch strings.ToLower(formatStr) {
	case "json":
		handler = slog.NewJSONHandler(out, opts)
	case "text":
		handler = slog.NewTextHandler(out, opts)
	default:
		return fmt.Errorf("invalid log format %q: must be json or text", formatStr)
	}

	slog.SetDefault(slog.New(handler))
	return nil
}

func init() {
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().StringVar(&logFormat, "log-format", "json", "Log output format (json, text)")

	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(authCmd)
	rootCmd.AddCommand(versionCmd)
}
