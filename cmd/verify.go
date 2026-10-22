package cmd

import (
	"fmt"
	"os"
	"os/signal"

	"github.com/miskawu/dbshift/internal/migrator"

	"github.com/spf13/cobra"
)

var verifyCmd = &cobra.Command{
	Use:     "verify",
	Short:   "驗證 MySQL 與 PostgreSQL 間的資料一致性",
	Example: `  dbshift verify --databases mydb`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(cfg.Databases) == 0 {
			return fmt.Errorf("--databases flag is required")
		}

		logger := newLogger(logLevel)

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		m := migrator.NewMigrator(cfg, logger)
		defer m.Close()

		results, err := m.Verify(ctx)
		if err != nil {
			return err
		}

		migrator.PrintVerifyResults(os.Stdout, results)

		if migrator.HasMismatch(results) {
			return fmt.Errorf("verification found mismatches")
		}

		return nil
	},
}
