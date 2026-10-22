package migrator

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/miskawu/dbshift/internal/config"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// VerifyResult holds verification results for one database.
type VerifyResult struct {
	Database string
	Tables   []TableResult
}

// TableResult holds verification results for one table.
type TableResult struct {
	Table         string
	MySQLCount    int64
	PGCount       int64
	CountMatch    bool
	ChecksumMatch *bool // nil if table has no checksum-able columns
	Status        string
	Diagnosis     *DiagnosisResult // nil unless CHECKSUM_MISMATCH and diagnosis enabled
}

// Verify compares row counts and checksums between MySQL and PG.
func (m *Migrator) Verify(ctx context.Context) ([]VerifyResult, error) {
	var results []VerifyResult

	for _, mapping := range config.ParseDatabaseMappings(m.cfg.Databases) {
		r, err := m.verifyDatabase(ctx, mapping)
		if err != nil {
			return nil, fmt.Errorf("verify %s→%s: %w", mapping.MySQL, mapping.PG, err)
		}
		results = append(results, r)
	}

	return results, nil
}

func (m *Migrator) verifyDatabase(ctx context.Context, mapping config.DatabaseMapping) (VerifyResult, error) {
	result := VerifyResult{Database: mapping.MySQL + "→" + mapping.PG}

	mysqlDB, err := sql.Open("mysql", m.cfg.MySQL.MySQLDSN(mapping.MySQL))
	if err != nil {
		return result, fmt.Errorf("open mysql: %w", err)
	}
	defer mysqlDB.Close()

	pgPool, err := pgxpool.New(ctx, m.cfg.PG.PostgresDSN(mapping.PG))
	if err != nil {
		return result, fmt.Errorf("connect pg: %w", err)
	}
	defer pgPool.Close()

	tables, err := listTables(ctx, mysqlDB, mapping.MySQL)
	if err != nil {
		return result, fmt.Errorf("list tables: %w", err)
	}

	// Filter to tables that also exist in PG
	pgTables, err := listPGTables(ctx, pgPool)
	if err != nil {
		return result, fmt.Errorf("list pg tables: %w", err)
	}

	pgTableSet := make(map[string]bool, len(pgTables))
	for _, t := range pgTables {
		pgTableSet[t] = true
	}

	var verifyTables []string
	for _, t := range tables {
		if pgTableSet[t] {
			verifyTables = append(verifyTables, t)
		} else {
			m.logger.Warn("資料表在 PostgreSQL 中不存在，略過驗證", "table", t)
		}
	}
	tables = verifyTables

	// Parallel table verification with bounded workers
	type indexedResult struct {
		index int
		tr    TableResult
	}

	sem := make(chan struct{}, m.cfg.Workers)
	ch := make(chan indexedResult, len(tables))
	var wg sync.WaitGroup

loop:
	for i, table := range tables {
		select {
		case <-ctx.Done():
			break loop
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(idx int, t string) {
			defer wg.Done()
			defer func() { <-sem }()

			tr, verifyErr := m.verifyTable(ctx, mysqlDB, pgPool, mapping.MySQL, t)
			if verifyErr != nil {
				m.logger.Warn("驗證資料表失敗", "table", t, "error", verifyErr)
				tr = TableResult{Table: t, Status: "ERROR"}
			}
			ch <- indexedResult{index: idx, tr: tr}
		}(i, table)
	}

	wg.Wait()
	close(ch)

	ordered := make([]TableResult, len(tables))
	n := 0
	for r := range ch {
		ordered[r.index] = r.tr
		n++
	}
	result.Tables = ordered[:n]

	if ctx.Err() != nil {
		return result, fmt.Errorf("interrupted: %w", ctx.Err())
	}

	return result, nil
}

func (m *Migrator) verifyTable(ctx context.Context, mysqlDB *sql.DB, pgPool *pgxpool.Pool, dbName, table string) (TableResult, error) {
	tr := TableResult{Table: table}

	// Get column metadata (single query)
	numericCols, textCols, dateCols, err := getColumnsByType(ctx, mysqlDB, dbName, table)
	if err != nil {
		return tr, fmt.Errorf("get column types: %w", err)
	}

	pgColTypes, err := getPGColumnTypes(ctx, pgPool, table)
	if err != nil {
		return tr, fmt.Errorf("get pg column types: %w", err)
	}

	// Filter out MySQL columns that don't exist in PG (e.g. deprecated GORM fields)
	numericCols = filterExistingCols(numericCols, pgColTypes)
	textCols = filterExistingCols(textCols, pgColTypes)
	dateCols = filterExistingCols(dateCols, pgColTypes)

	// Run MySQL and PG queries in parallel
	type sideResult struct {
		count    int64
		numeric  string
		text     string
		date     string
		countErr error
		numErr   error
		textErr  error
		dateErr  error
	}

	var mRes, pRes sideResult
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		mRes.countErr = mysqlDB.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM `%s`", table)).Scan(&mRes.count)
		if mRes.countErr != nil {
			return
		}
		if len(numericCols) > 0 {
			mRes.numeric, mRes.numErr = mysqlNumericChecksum(ctx, mysqlDB, table, numericCols)
		}
		if len(textCols) > 0 {
			mRes.text, mRes.textErr = mysqlTextChecksum(ctx, mysqlDB, table, textCols)
		}
		if len(dateCols) > 0 {
			mRes.date, mRes.dateErr = mysqlDateChecksum(ctx, mysqlDB, table, dateCols)
		}
	}()

	go func() {
		defer wg.Done()
		sanitized := pgx.Identifier{table}.Sanitize()
		pRes.countErr = pgPool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+sanitized).Scan(&pRes.count)
		if pRes.countErr != nil {
			return
		}
		if len(numericCols) > 0 {
			pRes.numeric, pRes.numErr = pgNumericChecksum(ctx, pgPool, table, numericCols, pgColTypes)
		}
		if len(textCols) > 0 {
			pRes.text, pRes.textErr = pgTextChecksum(ctx, pgPool, table, textCols)
		}
		if len(dateCols) > 0 {
			pRes.date, pRes.dateErr = pgDateChecksum(ctx, pgPool, table, dateCols)
		}
	}()

	wg.Wait()

	if mRes.countErr != nil {
		return tr, fmt.Errorf("mysql count: %w", mRes.countErr)
	}
	if pRes.countErr != nil {
		return tr, fmt.Errorf("pg count: %w", pRes.countErr)
	}

	tr.MySQLCount = mRes.count
	tr.PGCount = pRes.count
	tr.CountMatch = tr.MySQLCount == tr.PGCount

	// Combine checksum results (log errors, skip failed checksums)
	var checksumParts []bool

	if len(numericCols) > 0 {
		if mRes.numErr != nil {
			m.logger.Warn("MySQL 數值校驗碼計算失敗", "table", table, "error", mRes.numErr)
		} else if pRes.numErr != nil {
			m.logger.Warn("PostgreSQL 數值校驗碼計算失敗", "table", table, "error", pRes.numErr)
		} else {
			checksumParts = append(checksumParts, mRes.numeric == pRes.numeric)
		}
	}

	if len(textCols) > 0 {
		if mRes.textErr != nil {
			m.logger.Warn("MySQL 文字校驗碼計算失敗", "table", table, "error", mRes.textErr)
		} else if pRes.textErr != nil {
			m.logger.Warn("PostgreSQL 文字校驗碼計算失敗", "table", table, "error", pRes.textErr)
		} else {
			checksumParts = append(checksumParts, mRes.text == pRes.text)
		}
	}

	if len(dateCols) > 0 {
		if mRes.dateErr != nil {
			m.logger.Warn("MySQL 日期校驗碼計算失敗", "table", table, "error", mRes.dateErr)
		} else if pRes.dateErr != nil {
			m.logger.Warn("PostgreSQL 日期校驗碼計算失敗", "table", table, "error", pRes.dateErr)
		} else {
			checksumParts = append(checksumParts, mRes.date == pRes.date)
		}
	}

	tr.ChecksumMatch = combineChecksumResults(checksumParts)
	tr.Status = computeStatus(tr)

	// Drill-down diagnosis on checksum mismatch
	if tr.ChecksumMatch != nil && !*tr.ChecksumMatch && !m.cfg.SkipDiagnosis {
		diag, diagErr := m.diagnoseTable(ctx, mysqlDB, pgPool, dbName, table,
			numericCols, textCols, dateCols, pgColTypes)
		if diagErr != nil {
			m.logger.Warn("診斷分析失敗", "table", table, "error", diagErr)
		} else {
			tr.Diagnosis = diag
		}
	}

	return tr, nil
}

// combineChecksumResults returns nil if no parts, or a pointer to the AND of all parts.
func combineChecksumResults(parts []bool) *bool {
	if len(parts) == 0 {
		return nil
	}
	allMatch := true
	for _, p := range parts {
		if !p {
			allMatch = false
			break
		}
	}
	return &allMatch
}

func getColumnsByType(ctx context.Context, db *sql.DB, dbName, table string) (numeric, text, date []string, err error) {
	rows, err := db.QueryContext(ctx,
		`SELECT column_name, data_type FROM information_schema.columns
		 WHERE table_schema = ? AND table_name = ?
		 ORDER BY ordinal_position`, dbName, table)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var name, dtype string
		if err := rows.Scan(&name, &dtype); err != nil {
			return nil, nil, nil, err
		}
		switch dtype {
		case "int", "bigint", "smallint", "mediumint", "tinyint":
			numeric = append(numeric, name)
		case "varchar", "char", "text", "tinytext", "mediumtext", "longtext":
			text = append(text, name)
		case "date", "datetime", "timestamp":
			date = append(date, name)
		}
	}
	return numeric, text, date, rows.Err()
}

func mysqlNumericChecksum(ctx context.Context, db *sql.DB, table string, cols []string) (string, error) {
	parts := make([]string, len(cols))
	for i, col := range cols {
		parts[i] = fmt.Sprintf("CAST(COALESCE(SUM(`%s`), 0) AS CHAR)", col)
	}

	query := fmt.Sprintf("SELECT CONCAT(%s) FROM `%s`",
		strings.Join(parts, ", '|', "), table)

	var checksum string
	if err := db.QueryRowContext(ctx, query).Scan(&checksum); err != nil {
		return "", err
	}
	return checksum, nil
}

func pgNumericChecksum(ctx context.Context, pool *pgxpool.Pool, table string, cols []string, pgTypes map[string]string) (string, error) {
	parts := make([]string, len(cols))
	for i, col := range cols {
		sanitized := pgx.Identifier{col}.Sanitize()
		if strings.ToLower(pgTypes[col]) == "boolean" {
			parts[i] = fmt.Sprintf("COALESCE(SUM(%s::int), 0)", sanitized)
		} else {
			parts[i] = fmt.Sprintf("COALESCE(SUM(%s::bigint), 0)", sanitized)
		}
	}

	query := fmt.Sprintf("SELECT CONCAT(%s) FROM %s",
		strings.Join(parts, ", '|', "), pgx.Identifier{table}.Sanitize())

	var checksum string
	if err := pool.QueryRow(ctx, query).Scan(&checksum); err != nil {
		return "", err
	}
	return checksum, nil
}

func mysqlTextChecksum(ctx context.Context, db *sql.DB, table string, cols []string) (string, error) {
	parts := make([]string, len(cols))
	for i, col := range cols {
		parts[i] = fmt.Sprintf("COALESCE(SUM(CHAR_LENGTH(`%s`)), 0)", col)
	}

	query := fmt.Sprintf("SELECT CONCAT(%s) FROM `%s`",
		strings.Join(parts, ", '|', "), table)

	var checksum string
	if err := db.QueryRowContext(ctx, query).Scan(&checksum); err != nil {
		return "", err
	}
	return checksum, nil
}

func pgTextChecksum(ctx context.Context, pool *pgxpool.Pool, table string, cols []string) (string, error) {
	parts := make([]string, len(cols))
	for i, col := range cols {
		parts[i] = fmt.Sprintf("COALESCE(SUM(LENGTH(%s)), 0)", pgx.Identifier{col}.Sanitize())
	}

	query := fmt.Sprintf("SELECT CONCAT(%s) FROM %s",
		strings.Join(parts, ", '|', "), pgx.Identifier{table}.Sanitize())

	var checksum string
	if err := pool.QueryRow(ctx, query).Scan(&checksum); err != nil {
		return "", err
	}
	return checksum, nil
}

func mysqlDateChecksum(ctx context.Context, db *sql.DB, table string, cols []string) (string, error) {
	parts := make([]string, len(cols))
	for i, col := range cols {
		parts[i] = fmt.Sprintf(
			"CAST(COALESCE(UNIX_TIMESTAMP(MIN(`%s`)), 0) AS SIGNED), '|', CAST(COALESCE(UNIX_TIMESTAMP(MAX(`%s`)), 0) AS SIGNED)",
			col, col)
	}

	query := fmt.Sprintf("SELECT CONCAT(%s) FROM `%s`",
		strings.Join(parts, ", '|', "), table)

	var checksum string
	if err := db.QueryRowContext(ctx, query).Scan(&checksum); err != nil {
		return "", err
	}
	return checksum, nil
}

func pgDateChecksum(ctx context.Context, pool *pgxpool.Pool, table string, cols []string) (string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL TIME ZONE 'UTC'"); err != nil {
		return "", fmt.Errorf("set timezone: %w", err)
	}

	parts := make([]string, len(cols))
	for i, col := range cols {
		sanitized := pgx.Identifier{col}.Sanitize()
		parts[i] = fmt.Sprintf(
			"COALESCE(EXTRACT(EPOCH FROM MIN(%s))::bigint, 0), '|', COALESCE(EXTRACT(EPOCH FROM MAX(%s))::bigint, 0)",
			sanitized, sanitized)
	}

	query := fmt.Sprintf("SELECT CONCAT(%s) FROM %s",
		strings.Join(parts, ", '|', "), pgx.Identifier{table}.Sanitize())

	var checksum string
	if err := tx.QueryRow(ctx, query).Scan(&checksum); err != nil {
		return "", err
	}
	return checksum, nil
}

// filterExistingCols returns only the columns that exist in the PG column type map.
func filterExistingCols(cols []string, pgCols map[string]string) []string {
	var out []string
	for _, c := range cols {
		if _, ok := pgCols[c]; ok {
			out = append(out, c)
		}
	}
	return out
}

func computeStatus(tr TableResult) string {
	if !tr.CountMatch {
		return "COUNT_MISMATCH"
	}
	if tr.ChecksumMatch != nil && !*tr.ChecksumMatch {
		return "CHECKSUM_MISMATCH"
	}
	return "OK"
}

// HasMismatch returns true if any table has a non-OK status.
func HasMismatch(results []VerifyResult) bool {
	for _, r := range results {
		for _, t := range r.Tables {
			if t.Status != "OK" {
				return true
			}
		}
	}
	return false
}

// PrintVerifyResults writes a formatted verification report to w.
func PrintVerifyResults(w io.Writer, results []VerifyResult) {
	for _, r := range results {
		fmt.Fprintf(w, "\n--- %s ---\n\n", r.Database)
		fmt.Fprintf(w, "%-40s %10s %10s %12s %s\n", "TABLE", "M_COUNT", "P_COUNT", "CHECKSUM", "STATUS")
		fmt.Fprintf(w, "%-40s %10s %10s %12s %s\n", strings.Repeat("-", 5), strings.Repeat("-", 7), strings.Repeat("-", 7), strings.Repeat("-", 8), strings.Repeat("-", 6))

		for _, t := range r.Tables {
			checksumStr := "n/a"
			if t.ChecksumMatch != nil {
				if *t.ChecksumMatch {
					checksumStr = "MATCH"
				} else {
					checksumStr = "MISMATCH"
				}
			}
			fmt.Fprintf(w, "%-40s %10d %10d %12s %s\n", t.Table, t.MySQLCount, t.PGCount, checksumStr, t.Status)
			if t.Diagnosis != nil {
				printDiagnosis(w, t.Diagnosis)
			}
		}

		if HasMismatch([]VerifyResult{r}) {
			fmt.Fprintf(w, "\nWARNING: %s has data mismatches\n", r.Database)
		} else {
			fmt.Fprintf(w, "\n%s verification passed\n", r.Database)
		}
	}
}
