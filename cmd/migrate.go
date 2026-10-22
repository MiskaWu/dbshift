package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/miskawu/dbshift/internal/migrator"

	"github.com/spf13/cobra"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "從 MySQL 遷移資料至 PostgreSQL",
	Example: `  dbshift migrate --databases mydb,mydb_log --mysql-host 10.0.0.1 --pg-host 10.0.0.2
  dbshift migrate --databases mydb --batch 5000 --workers 8`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(cfg.Databases) == 0 {
			return fmt.Errorf("--databases flag is required")
		}

		logger := newLogger(logLevel)

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		m := migrator.NewMigrator(cfg, logger)
		defer m.Close()

		return m.Run(ctx)
	},
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
