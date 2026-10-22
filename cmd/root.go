package cmd

import (
	"os"

	"github.com/miskawu/dbshift/internal/config"

	"github.com/spf13/cobra"
)

var (
	cfg      = &config.Config{}
	logLevel string
)

var rootCmd = &cobra.Command{
	Use:   "dbshift",
	Short: "MySQL 至 PostgreSQL 資料遷移工具",
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	pf := rootCmd.PersistentFlags()

	pf.StringVar(&cfg.MySQL.Host, "mysql-host", config.EnvOr("MYSQL_HOST", "127.0.0.1"), "MySQL host")
	pf.IntVar(&cfg.MySQL.Port, "mysql-port", config.EnvOrInt("MYSQL_PORT", 3306), "MySQL port")
	pf.StringVar(&cfg.MySQL.User, "mysql-user", config.EnvOr("MYSQL_USER", "root"), "MySQL user")
	pf.StringVar(&cfg.MySQL.Pass, "mysql-pass", config.EnvOr("MYSQL_PASS", ""), "MySQL password")

	pf.StringVar(&cfg.PG.Host, "pg-host", config.EnvOr("PG_HOST", "127.0.0.1"), "PostgreSQL host")
	pf.IntVar(&cfg.PG.Port, "pg-port", config.EnvOrInt("PG_PORT", 5432), "PostgreSQL port")
	pf.StringVar(&cfg.PG.User, "pg-user", config.EnvOr("PG_USER", "postgres"), "PostgreSQL user")
	pf.StringVar(&cfg.PG.Pass, "pg-pass", config.EnvOr("PG_PASS", ""), "PostgreSQL password")
	pf.StringVar(&cfg.PG.SSLMode, "pg-ssl", config.EnvOr("PG_SSLMODE", "disable"), "PostgreSQL SSL mode (disable, require, verify-ca, verify-full)")

	pf.StringSliceVar(&cfg.Databases, "databases", nil, "Databases to migrate (comma-separated, required)")
	pf.IntVar(&cfg.BatchSize, "batch", 1000, "Batch size for COPY")
	pf.IntVar(&cfg.Workers, "workers", 4, "Concurrent table workers")
	pf.IntVar(&cfg.DiagnoseLimit, "diagnose-limit", 10, "Max differing rows per table in diagnosis")
	pf.BoolVar(&cfg.SkipDiagnosis, "skip-diagnosis", false, "Skip per-column/per-row diagnosis on mismatch")
	pf.StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")

	rootCmd.AddCommand(migrateCmd, verifyCmd)
}
