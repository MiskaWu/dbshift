package migrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miskawu/dbshift/internal/config"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migrator orchestrates data migration from MySQL to PostgreSQL.
type Migrator struct {
	cfg    *config.Config
	logger *slog.Logger
}

// NewMigrator creates a new Migrator.
func NewMigrator(cfg *config.Config, logger *slog.Logger) *Migrator {
	return &Migrator{cfg: cfg, logger: logger}
}

// Close releases any resources held by the Migrator.
func (m *Migrator) Close() {}

// Run executes the full migration: transfer data then verify.
func (m *Migrator) Run(ctx context.Context) error {
	for _, mapping := range config.ParseDatabaseMappings(m.cfg.Databases) {
		m.logger.Info("========================================")
		m.logger.Info("開始資料庫遷移", "mysql", mapping.MySQL, "pg", mapping.PG)
		m.logger.Info("========================================")

		if err := m.migrateDatabase(ctx, mapping); err != nil {
			return fmt.Errorf("database %s→%s: %w", mapping.MySQL, mapping.PG, err)
		}

		m.logger.Info("資料庫遷移完成", "mysql", mapping.MySQL, "pg", mapping.PG)
	}

	m.logger.Info("所有資料庫遷移完成，執行驗證")
	results, err := m.Verify(ctx)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	PrintVerifyResults(os.Stdout, results)
	if HasMismatch(results) {
		return fmt.Errorf("verification found mismatches")
	}

	return nil
}

func (m *Migrator) migrateDatabase(ctx context.Context, mapping config.DatabaseMapping) error {
	mysqlDB, err := sql.Open("mysql", m.cfg.MySQL.MySQLDSN(mapping.MySQL))
	if err != nil {
		return fmt.Errorf("open mysql: %w", err)
	}
	defer mysqlDB.Close()

	if err := mysqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping mysql: %w", err)
	}

	pgPool, err := pgxpool.New(ctx, m.cfg.PG.PostgresDSN(mapping.PG))
	if err != nil {
		return fmt.Errorf("connect pg: %w", err)
	}
	defer pgPool.Close()

	// List MySQL tables
	tables, err := listTables(ctx, mysqlDB, mapping.MySQL)
	if err != nil {
		return fmt.Errorf("list mysql tables: %w", err)
	}

	// Filter to tables that also exist in PG
	pgTables, err := listPGTables(ctx, pgPool)
	if err != nil {
		return fmt.Errorf("list pg tables: %w", err)
	}

	pgTableSet := make(map[string]bool, len(pgTables))
	for _, t := range pgTables {
		pgTableSet[t] = true
	}

	var migrateTables []string
	for _, t := range tables {
		if pgTableSet[t] {
			migrateTables = append(migrateTables, t)
		} else {
			m.logger.Warn("資料表在 PostgreSQL 中不存在，略過", "table", t)
		}
	}

	m.logger.Info("待遷移資料表",
		"mysql_total", len(tables),
		"pg_total", len(pgTables),
		"to_migrate", len(migrateTables),
	)

	// Migrate concurrently with bounded workers
	gCtx, gCancel := context.WithCancel(ctx)
	defer gCancel()

	sem := make(chan struct{}, m.cfg.Workers)
	errs := make(chan error, len(migrateTables))
	var wg sync.WaitGroup

loop:
	for _, table := range migrateTables {
		select {
		case <-gCtx.Done():
			break loop
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(t string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := m.migrateTable(gCtx, mysqlDB, pgPool, t); err != nil {
				gCancel()
				errs <- fmt.Errorf("table %s: %w", t, err)
			}
		}(table)
	}

	wg.Wait()
	close(errs)

	var allErrs []error
	for err := range errs {
		allErrs = append(allErrs, err)
	}

	if ctx.Err() != nil {
		m.logger.Warn("遷移已中斷，等待進行中的工作結束")
		allErrs = append(allErrs, fmt.Errorf("interrupted: %w", ctx.Err()))
	}

	return errors.Join(allErrs...)
}

func (m *Migrator) migrateTable(ctx context.Context, mysqlDB *sql.DB, pgPool *pgxpool.Pool, table string) error {
	m.logger.Info("正在遷移資料表", "table", table)

	sanitizedTable := pgx.Identifier{table}.Sanitize()

	// Disable triggers outside transaction (DDL, may fail without superuser)
	if _, err := pgPool.Exec(ctx, "ALTER TABLE "+sanitizedTable+" DISABLE TRIGGER ALL"); err != nil {
		m.logger.Warn("停用觸發器失敗（可能需要超級使用者權限）", "table", table, "error", err)
	}
	defer pgPool.Exec(context.Background(), "ALTER TABLE "+sanitizedTable+" ENABLE TRIGGER ALL")

	// Stream from MySQL
	rows, err := mysqlDB.QueryContext(ctx, "SELECT * FROM `"+table+"`")
	if err != nil {
		return fmt.Errorf("query mysql: %w", err)
	}
	defer rows.Close()

	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return fmt.Errorf("column types: %w", err)
	}

	colNames := make([]string, len(colTypes))
	colTypeNames := make([]string, len(colTypes))
	for i, ct := range colTypes {
		colNames[i] = ct.Name()
		colTypeNames[i] = ct.DatabaseTypeName()
	}

	// Get PG column types for cross-type conversion (e.g. TINYINT → boolean)
	pgColTypeMap, err := getPGColumnTypes(ctx, pgPool, table)
	if err != nil {
		return fmt.Errorf("get pg column types: %w", err)
	}

	// Filter out MySQL columns that don't exist in PG (e.g. deprecated GORM fields)
	keepIndices, skippedCols := filterColumns(colNames, pgColTypeMap)
	for _, col := range skippedCols {
		m.logger.Warn("來源欄位在目標不存在，略過", "table", table, "column", col)
	}

	filteredColNames := make([]string, len(keepIndices))
	filteredColTypeNames := make([]string, len(keepIndices))
	filteredPGColTypes := make([]string, len(keepIndices))
	for i, idx := range keepIndices {
		filteredColNames[i] = colNames[idx]
		filteredColTypeNames[i] = colTypeNames[idx]
		filteredPGColTypes[i] = pgColTypeMap[colNames[idx]]
	}

	// Begin PG transaction: TRUNCATE + all COPY batches + reset sequences are atomic.
	// This prevents TRUNCATE CASCADE from other concurrent workers from interfering.
	tx, err := pgPool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(context.Background())

	// Truncate within transaction
	if _, err := tx.Exec(ctx, "TRUNCATE TABLE "+sanitizedTable+" CASCADE"); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}

	// Batch and COPY within transaction
	var totalRows int64
	batch := make([][]interface{}, 0, m.cfg.BatchSize)

	for rows.Next() {
		// Scan all MySQL columns
		values := make([]interface{}, len(colTypes))
		ptrs := make([]interface{}, len(colTypes))
		for i := range values {
			ptrs[i] = &values[i]
		}

		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}

		// Keep only columns that exist in PG
		filtered := make([]interface{}, len(keepIndices))
		for i, idx := range keepIndices {
			filtered[i] = convertMySQLValue(values[idx], filteredColTypeNames[i], filteredPGColTypes[i])
		}

		batch = append(batch, filtered)

		if len(batch) >= m.cfg.BatchSize {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("interrupted: %w", err)
			}
			n, err := tx.CopyFrom(ctx, pgx.Identifier{table}, filteredColNames, pgx.CopyFromRows(batch))
			if err != nil {
				return fmt.Errorf("copy batch at row %d: %w", totalRows, err)
			}
			totalRows += n
			batch = batch[:0]
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	if len(batch) > 0 {
		n, err := tx.CopyFrom(ctx, pgx.Identifier{table}, filteredColNames, pgx.CopyFromRows(batch))
		if err != nil {
			return fmt.Errorf("copy remaining: %w", err)
		}
		totalRows += n
	}

	// Reset sequences within transaction
	if err := resetSequences(ctx, tx, table); err != nil {
		m.logger.Warn("重設序列失敗", "table", table, "error", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	m.logger.Info("資料表遷移完成", "table", table, "rows", totalRows)
	return nil
}

func listTables(ctx context.Context, db *sql.DB, dbName string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = ? AND table_type = 'BASE TABLE'
		 ORDER BY table_name`, dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func listPGTables(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

// convertMySQLValue converts a MySQL scanned value to a type compatible with pgx COPY.
// pgType is the PostgreSQL target column data_type (e.g. "boolean", "integer").
func convertMySQLValue(v interface{}, mysqlType, pgType string) interface{} {
	if v == nil {
		return nil
	}

	// PG-specific target type conversions for pgx binary COPY.
	switch strings.ToLower(pgType) {
	case "boolean":
		return toBool(v)
	case "time without time zone", "time with time zone":
		return toTimeOfDay(v)
	}

	b, ok := v.([]byte)
	if !ok {
		return v
	}

	switch strings.ToUpper(mysqlType) {
	case "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB", "BINARY", "VARBINARY":
		return b
	case "BIT":
		if len(b) == 1 {
			return b[0] != 0
		}
		var n int64
		for _, x := range b {
			n = n<<8 | int64(x)
		}
		return n
	default:
		// VARCHAR, TEXT, CHAR, JSON, DECIMAL, ENUM, SET, etc.
		return string(b)
	}
}

// toTimeOfDay converts a MySQL TIME string to time.Time for PG time columns.
func toTimeOfDay(v interface{}) interface{} {
	var s string
	switch val := v.(type) {
	case time.Time:
		return val
	case []byte:
		s = string(val)
	case string:
		s = val
	default:
		return v
	}

	for _, layout := range []string{"15:04:05.999999", "15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return v
}

// toBool converts a MySQL value to a Go bool for PG boolean columns.
func toBool(v interface{}) interface{} {
	switch val := v.(type) {
	case bool:
		return val
	case int64:
		return val != 0
	case float64:
		return val != 0
	case []byte:
		return len(val) > 0 && val[0] != '0' && val[0] != 0
	case string:
		return val != "" && val != "0"
	default:
		return v
	}
}

// filterColumns returns the indices of MySQL columns that exist in PG,
// and the names of columns that were skipped (exist in MySQL but not PG).
func filterColumns(mysqlColNames []string, pgColTypeMap map[string]string) (keepIndices []int, skippedCols []string) {
	for i, name := range mysqlColNames {
		if _, ok := pgColTypeMap[name]; ok {
			keepIndices = append(keepIndices, i)
		} else {
			skippedCols = append(skippedCols, name)
		}
	}
	return
}

func getPGColumnTypes(ctx context.Context, pool *pgxpool.Pool, table string) (map[string]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT column_name, data_type FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1
		 ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	types := make(map[string]string)
	for rows.Next() {
		var name, dtype string
		if err := rows.Scan(&name, &dtype); err != nil {
			return nil, err
		}
		types[name] = dtype
	}
	return types, rows.Err()
}

func resetSequences(ctx context.Context, tx pgx.Tx, table string) error {
	rows, err := tx.Query(ctx, `
		SELECT column_name, pg_get_serial_sequence($1, column_name)
		FROM information_schema.columns
		WHERE table_name = $1 AND table_schema = 'public'
		  AND column_default LIKE 'nextval%'`, table)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Collect all sequences first to avoid "conn busy" —
	// pgx.Tx shares a single connection, so we cannot Exec while rows are open.
	type seqInfo struct {
		colName, seqName string
	}
	var seqs []seqInfo
	for rows.Next() {
		var colName, seqName string
		if err := rows.Scan(&colName, &seqName); err != nil {
			return err
		}
		if seqName != "" {
			seqs = append(seqs, seqInfo{colName, seqName})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	for _, s := range seqs {
		query := fmt.Sprintf(
			`SELECT setval($1, COALESCE((SELECT MAX(%s) FROM %s), 1))`,
			pgx.Identifier{s.colName}.Sanitize(),
			pgx.Identifier{table}.Sanitize(),
		)
		if _, err := tx.Exec(ctx, query, s.seqName); err != nil {
			return fmt.Errorf("reset sequence %s: %w", s.seqName, err)
		}
	}

	return nil
}
