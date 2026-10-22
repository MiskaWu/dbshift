package migrator

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ColumnMismatch describes a single column whose checksum differs.
type ColumnMismatch struct {
	Column     string
	Category   string // "numeric", "text", "date"
	MySQLValue string
	PGValue    string
}

// RowDiff describes a single row where a column value differs.
type RowDiff struct {
	PK         map[string]string
	Column     string
	MySQLValue *string // nil means NULL
	PGValue    *string // nil means NULL
}

// DiagnosisResult holds drill-down diagnosis for a table with CHECKSUM_MISMATCH.
type DiagnosisResult struct {
	Columns    []ColumnMismatch
	RowDiffs   []RowDiff
	PKColumns  []string
	NoPK       bool
	RowDiffCap int
	TotalDiffs int
}

// --- Pure logic functions ---

// comparePerColumnChecksums compares per-column checksum maps and returns mismatches.
func comparePerColumnChecksums(mysql, pg map[string]string, category string) []ColumnMismatch {
	var out []ColumnMismatch
	// Collect all column names from mysql map
	cols := make([]string, 0, len(mysql))
	for col := range mysql {
		cols = append(cols, col)
	}
	sort.Strings(cols)

	for _, col := range cols {
		mVal := mysql[col]
		pVal, ok := pg[col]
		if !ok {
			pVal = "(missing)"
		}
		if mVal != pVal {
			out = append(out, ColumnMismatch{
				Column:     col,
				Category:   category,
				MySQLValue: mVal,
				PGValue:    pVal,
			})
		}
	}
	return out
}

// printDiagnosis writes diagnosis details to w.
func printDiagnosis(w io.Writer, d *DiagnosisResult) {
	if len(d.Columns) > 0 {
		fmt.Fprintf(w, "\n  Mismatching columns:\n")
		fmt.Fprintf(w, "  %-30s %-10s %20s %20s\n", "COLUMN", "TYPE", "MYSQL", "PG")
		for _, cm := range d.Columns {
			fmt.Fprintf(w, "  %-30s %-10s %20s %20s\n",
				cm.Column, cm.Category, cm.MySQLValue, cm.PGValue)
		}
	}

	if d.NoPK {
		fmt.Fprintf(w, "\n  (No primary key found; per-row comparison skipped)\n")
	} else if len(d.RowDiffs) > 0 {
		fmt.Fprintf(w, "\n  Sample differing rows (up to %d):\n", d.RowDiffCap)
		for _, rd := range d.RowDiffs {
			pkStr := formatPK(rd.PK, d.PKColumns)
			fmt.Fprintf(w, "    PK(%s)  col=%s  mysql=%s  pg=%s\n",
				pkStr, rd.Column, nullStr(rd.MySQLValue), nullStr(rd.PGValue))
		}
		remaining := d.TotalDiffs - len(d.RowDiffs)
		if remaining > 0 {
			fmt.Fprintf(w, "    ... and %d more differing rows\n", remaining)
		}
	}
	fmt.Fprintln(w)
}

func formatPK(pk map[string]string, pkCols []string) string {
	parts := make([]string, 0, len(pkCols))
	for _, col := range pkCols {
		parts = append(parts, fmt.Sprintf("%s=%s", col, pk[col]))
	}
	return strings.Join(parts, ", ")
}

func nullStr(s *string) string {
	if s == nil {
		return "NULL"
	}
	return *s
}

// --- SQL builder functions (pure, no DB) ---

func buildMySQLPerColumnNumericQuery(table string, cols []string) string {
	parts := make([]string, len(cols))
	for i, col := range cols {
		parts[i] = fmt.Sprintf("CAST(COALESCE(SUM(`%s`), 0) AS CHAR) AS `%s`", col, col)
	}
	return fmt.Sprintf("SELECT %s FROM `%s`", strings.Join(parts, ", "), table)
}

func buildPGPerColumnNumericQuery(table string, cols []string, pgTypes map[string]string) string {
	parts := make([]string, len(cols))
	for i, col := range cols {
		sanitized := pgx.Identifier{col}.Sanitize()
		if strings.ToLower(pgTypes[col]) == "boolean" {
			parts[i] = fmt.Sprintf("COALESCE(SUM(%s::int), 0) AS %s", sanitized, sanitized)
		} else {
			parts[i] = fmt.Sprintf("COALESCE(SUM(%s::bigint), 0) AS %s", sanitized, sanitized)
		}
	}
	return fmt.Sprintf("SELECT %s FROM %s", strings.Join(parts, ", "), pgx.Identifier{table}.Sanitize())
}

func buildMySQLPerColumnTextQuery(table string, cols []string) string {
	parts := make([]string, len(cols))
	for i, col := range cols {
		parts[i] = fmt.Sprintf("COALESCE(SUM(CHAR_LENGTH(`%s`)), 0) AS `%s`", col, col)
	}
	return fmt.Sprintf("SELECT %s FROM `%s`", strings.Join(parts, ", "), table)
}

func buildPGPerColumnTextQuery(table string, cols []string) string {
	parts := make([]string, len(cols))
	for i, col := range cols {
		sanitized := pgx.Identifier{col}.Sanitize()
		parts[i] = fmt.Sprintf("COALESCE(SUM(LENGTH(%s)), 0) AS %s", sanitized, sanitized)
	}
	return fmt.Sprintf("SELECT %s FROM %s", strings.Join(parts, ", "), pgx.Identifier{table}.Sanitize())
}

func buildMySQLPerColumnDateQuery(table string, cols []string) string {
	parts := make([]string, len(cols))
	for i, col := range cols {
		parts[i] = fmt.Sprintf(
			"CAST(COALESCE(UNIX_TIMESTAMP(MIN(`%s`)), 0) AS SIGNED) AS `%s_min`, CAST(COALESCE(UNIX_TIMESTAMP(MAX(`%s`)), 0) AS SIGNED) AS `%s_max`",
			col, col, col, col)
	}
	return fmt.Sprintf("SELECT %s FROM `%s`", strings.Join(parts, ", "), table)
}

func buildPGPerColumnDateQuery(table string, cols []string) string {
	parts := make([]string, len(cols))
	for i, col := range cols {
		sanitized := pgx.Identifier{col}.Sanitize()
		parts[i] = fmt.Sprintf(
			"COALESCE(EXTRACT(EPOCH FROM MIN(%s))::bigint, 0) AS %s, COALESCE(EXTRACT(EPOCH FROM MAX(%s))::bigint, 0) AS %s",
			sanitized, pgx.Identifier{col + "_min"}.Sanitize(), sanitized, pgx.Identifier{col + "_max"}.Sanitize())
	}
	return fmt.Sprintf("SELECT %s FROM %s", strings.Join(parts, ", "), pgx.Identifier{table}.Sanitize())
}

// --- DB query functions ---

func mysqlPerColumnNumericChecksum(ctx context.Context, db *sql.DB, table string, cols []string) (map[string]string, error) {
	query := buildMySQLPerColumnNumericQuery(table, cols)
	return scanColumnValues(ctx, db, query, cols)
}

func pgPerColumnNumericChecksum(ctx context.Context, pool *pgxpool.Pool, table string, cols []string, pgTypes map[string]string) (map[string]string, error) {
	query := buildPGPerColumnNumericQuery(table, cols, pgTypes)
	return scanPGColumnValues(ctx, pool, query, cols)
}

func mysqlPerColumnTextChecksum(ctx context.Context, db *sql.DB, table string, cols []string) (map[string]string, error) {
	query := buildMySQLPerColumnTextQuery(table, cols)
	return scanColumnValues(ctx, db, query, cols)
}

func pgPerColumnTextChecksum(ctx context.Context, pool *pgxpool.Pool, table string, cols []string) (map[string]string, error) {
	query := buildPGPerColumnTextQuery(table, cols)
	return scanPGColumnValues(ctx, pool, query, cols)
}

func mysqlPerColumnDateChecksum(ctx context.Context, db *sql.DB, table string, cols []string) (map[string]string, error) {
	// Date query returns col_min, col_max pairs — combine into "min|max" per col
	query := buildMySQLPerColumnDateQuery(table, cols)
	dateCols := make([]string, 0, len(cols)*2)
	for _, col := range cols {
		dateCols = append(dateCols, col+"_min", col+"_max")
	}
	vals, err := scanColumnValues(ctx, db, query, dateCols)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(cols))
	for _, col := range cols {
		result[col] = vals[col+"_min"] + "|" + vals[col+"_max"]
	}
	return result, nil
}

func pgPerColumnDateChecksum(ctx context.Context, pool *pgxpool.Pool, table string, cols []string) (map[string]string, error) {
	query := buildPGPerColumnDateQuery(table, cols)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL TIME ZONE 'UTC'"); err != nil {
		return nil, fmt.Errorf("set timezone: %w", err)
	}

	dateCols := make([]string, 0, len(cols)*2)
	for _, col := range cols {
		dateCols = append(dateCols, col+"_min", col+"_max")
	}

	row := tx.QueryRow(ctx, query)
	dest := make([]interface{}, len(dateCols))
	strs := make([]string, len(dateCols))
	for i := range dest {
		dest[i] = &strs[i]
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}

	vals := make(map[string]string, len(dateCols))
	for i, col := range dateCols {
		vals[col] = strs[i]
	}

	result := make(map[string]string, len(cols))
	for _, col := range cols {
		result[col] = vals[col+"_min"] + "|" + vals[col+"_max"]
	}
	return result, nil
}

// scanColumnValues runs a single-row query on MySQL and scans each column by name.
func scanColumnValues(ctx context.Context, db *sql.DB, query string, cols []string) (map[string]string, error) {
	row := db.QueryRowContext(ctx, query)
	dest := make([]interface{}, len(cols))
	strs := make([]string, len(cols))
	for i := range dest {
		dest[i] = &strs[i]
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	result := make(map[string]string, len(cols))
	for i, col := range cols {
		result[col] = strs[i]
	}
	return result, nil
}

// scanPGColumnValues runs a single-row query on PG and scans each column by name.
func scanPGColumnValues(ctx context.Context, pool *pgxpool.Pool, query string, cols []string) (map[string]string, error) {
	row := pool.QueryRow(ctx, query)
	dest := make([]interface{}, len(cols))
	strs := make([]string, len(cols))
	for i := range dest {
		dest[i] = &strs[i]
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	result := make(map[string]string, len(cols))
	for i, col := range cols {
		result[col] = strs[i]
	}
	return result, nil
}

// --- PK detection ---

func getPrimaryKeyColumns(ctx context.Context, db *sql.DB, dbName, table string) ([]string, error) {
	// Try PRIMARY KEY first
	cols, err := queryKeyColumns(ctx, db, dbName, table, "PRIMARY KEY")
	if err != nil {
		return nil, err
	}
	if len(cols) > 0 {
		return cols, nil
	}
	// Fallback to first UNIQUE constraint
	return queryFirstUniqueColumns(ctx, db, dbName, table)
}

func queryKeyColumns(ctx context.Context, db *sql.DB, dbName, table, constraintType string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT kcu.column_name
		 FROM information_schema.table_constraints tc
		 JOIN information_schema.key_column_usage kcu
		   ON tc.constraint_name = kcu.constraint_name
		   AND tc.table_schema = kcu.table_schema
		   AND tc.table_name = kcu.table_name
		 WHERE tc.table_schema = ? AND tc.table_name = ?
		   AND tc.constraint_type = ?
		 ORDER BY kcu.ordinal_position`, dbName, table, constraintType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, err
		}
		cols = append(cols, col)
	}
	return cols, rows.Err()
}

func queryFirstUniqueColumns(ctx context.Context, db *sql.DB, dbName, table string) ([]string, error) {
	// Find the first UNIQUE constraint name
	var constraintName string
	err := db.QueryRowContext(ctx,
		`SELECT tc.constraint_name
		 FROM information_schema.table_constraints tc
		 WHERE tc.table_schema = ? AND tc.table_name = ?
		   AND tc.constraint_type = 'UNIQUE'
		 ORDER BY tc.constraint_name
		 LIMIT 1`, dbName, table).Scan(&constraintName)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx,
		`SELECT column_name
		 FROM information_schema.key_column_usage
		 WHERE table_schema = ? AND table_name = ? AND constraint_name = ?
		 ORDER BY ordinal_position`, dbName, table, constraintName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, err
		}
		cols = append(cols, col)
	}
	return cols, rows.Err()
}

// --- Per-row diff ---

func diagnoseRowDiffs(
	ctx context.Context,
	mysqlDB *sql.DB,
	pgPool *pgxpool.Pool,
	table string,
	pkCols []string,
	mismatchCols []string,
	limit int,
) (diffs []RowDiff, totalDiffs int, err error) {
	queryCols := make([]string, 0, len(pkCols)+len(mismatchCols))
	queryCols = append(queryCols, pkCols...)
	queryCols = append(queryCols, mismatchCols...)

	// Fetch from MySQL
	mysqlRows, err := fetchRows(ctx, mysqlDB, table, queryCols, pkCols)
	if err != nil {
		return nil, 0, fmt.Errorf("mysql fetch: %w", err)
	}

	// Fetch from PG
	pgRows, err := fetchPGRows(ctx, pgPool, table, queryCols, pkCols)
	if err != nil {
		return nil, 0, fmt.Errorf("pg fetch: %w", err)
	}

	// Compare
	for pkKey, mRow := range mysqlRows {
		pRow, ok := pgRows[pkKey]
		if !ok {
			// Row in MySQL but not in PG — shouldn't happen if counts match, but handle it
			for _, col := range mismatchCols {
				mVal := mRow[col]
				diffs = append(diffs, RowDiff{
					PK:         mRow.pkMap(pkCols),
					Column:     col,
					MySQLValue: mVal,
					PGValue:    nil,
				})
				totalDiffs++
			}
			continue
		}
		for _, col := range mismatchCols {
			mVal := mRow[col]
			pVal := pRow[col]
			if !strPtrEqual(mVal, pVal) {
				totalDiffs++
				if len(diffs) < limit {
					diffs = append(diffs, RowDiff{
						PK:         mRow.pkMap(pkCols),
						Column:     col,
						MySQLValue: mVal,
						PGValue:    pVal,
					})
				}
			}
		}
	}

	// Check for rows in PG but not in MySQL
	for pkKey, pRow := range pgRows {
		if _, ok := mysqlRows[pkKey]; !ok {
			for _, col := range mismatchCols {
				pVal := pRow[col]
				diffs = append(diffs, RowDiff{
					PK:         pRow.pkMap(pkCols),
					Column:     col,
					MySQLValue: nil,
					PGValue:    pVal,
				})
				totalDiffs++
			}
		}
	}

	return diffs, totalDiffs, nil
}

// rowData holds column values for a single row.
type rowData map[string]*string

func (r rowData) pkMap(pkCols []string) map[string]string {
	m := make(map[string]string, len(pkCols))
	for _, col := range pkCols {
		if v := r[col]; v != nil {
			m[col] = *v
		} else {
			m[col] = "NULL"
		}
	}
	return m
}

func strPtrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func fetchRows(ctx context.Context, db *sql.DB, table string, queryCols, pkCols []string) (map[string]rowData, error) {
	colExprs := make([]string, len(queryCols))
	for i, col := range queryCols {
		colExprs[i] = fmt.Sprintf("`%s`", col)
	}
	orderExprs := make([]string, len(pkCols))
	for i, col := range pkCols {
		orderExprs[i] = fmt.Sprintf("`%s`", col)
	}

	query := fmt.Sprintf("SELECT %s FROM `%s` ORDER BY %s",
		strings.Join(colExprs, ", "), table, strings.Join(orderExprs, ", "))

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]rowData)
	for rows.Next() {
		dest := make([]*string, len(queryCols))
		scanDest := make([]interface{}, len(queryCols))
		for i := range dest {
			scanDest[i] = &dest[i]
		}
		if err := rows.Scan(scanDest...); err != nil {
			return nil, err
		}
		rd := make(rowData, len(queryCols))
		for i, col := range queryCols {
			rd[col] = dest[i]
		}
		pkKey := buildPKKey(rd, pkCols)
		result[pkKey] = rd
	}
	return result, rows.Err()
}

func fetchPGRows(ctx context.Context, pool *pgxpool.Pool, table string, queryCols, pkCols []string) (map[string]rowData, error) {
	colExprs := make([]string, len(queryCols))
	for i, col := range queryCols {
		colExprs[i] = pgx.Identifier{col}.Sanitize()
	}
	orderExprs := make([]string, len(pkCols))
	for i, col := range pkCols {
		orderExprs[i] = pgx.Identifier{col}.Sanitize()
	}

	query := fmt.Sprintf("SELECT %s FROM %s ORDER BY %s",
		strings.Join(colExprs, ", "), pgx.Identifier{table}.Sanitize(), strings.Join(orderExprs, ", "))

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]rowData)
	for rows.Next() {
		dest := make([]*string, len(queryCols))
		scanDest := make([]interface{}, len(queryCols))
		for i := range dest {
			scanDest[i] = &dest[i]
		}
		if err := rows.Scan(scanDest...); err != nil {
			return nil, err
		}
		rd := make(rowData, len(queryCols))
		for i, col := range queryCols {
			rd[col] = dest[i]
		}
		pkKey := buildPKKey(rd, pkCols)
		result[pkKey] = rd
	}
	return result, rows.Err()
}

func buildPKKey(rd rowData, pkCols []string) string {
	parts := make([]string, len(pkCols))
	for i, col := range pkCols {
		if v := rd[col]; v != nil {
			parts[i] = *v
		} else {
			parts[i] = "\x00"
		}
	}
	return strings.Join(parts, "\x00\x01")
}

// --- Orchestrator ---

func (m *Migrator) diagnoseTable(
	ctx context.Context,
	mysqlDB *sql.DB,
	pgPool *pgxpool.Pool,
	dbName, table string,
	numericCols, textCols, dateCols []string,
	pgColTypes map[string]string,
) (*DiagnosisResult, error) {
	diag := &DiagnosisResult{
		RowDiffCap: m.cfg.DiagnoseLimit,
	}

	// Phase 1: per-column checksums (MySQL and PG in parallel)
	type checksumResult struct {
		mysql map[string]string
		pg    map[string]string
		err   error
	}

	var numRes, textRes, dateRes struct {
		mysql, pg  map[string]string
		mErr, pErr error
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if len(numericCols) > 0 {
			numRes.mysql, numRes.mErr = mysqlPerColumnNumericChecksum(ctx, mysqlDB, table, numericCols)
		}
		if len(textCols) > 0 {
			textRes.mysql, textRes.mErr = mysqlPerColumnTextChecksum(ctx, mysqlDB, table, textCols)
		}
		if len(dateCols) > 0 {
			dateRes.mysql, dateRes.mErr = mysqlPerColumnDateChecksum(ctx, mysqlDB, table, dateCols)
		}
	}()

	go func() {
		defer wg.Done()
		if len(numericCols) > 0 {
			numRes.pg, numRes.pErr = pgPerColumnNumericChecksum(ctx, pgPool, table, numericCols, pgColTypes)
		}
		if len(textCols) > 0 {
			textRes.pg, textRes.pErr = pgPerColumnTextChecksum(ctx, pgPool, table, textCols)
		}
		if len(dateCols) > 0 {
			dateRes.pg, dateRes.pErr = pgPerColumnDateChecksum(ctx, pgPool, table, dateCols)
		}
	}()

	wg.Wait()

	// Compare per-column checksums
	if numRes.mErr != nil {
		m.logger.Warn("MySQL 逐欄數值校驗碼失敗", "table", table, "error", numRes.mErr)
	} else if numRes.pErr != nil {
		m.logger.Warn("PG 逐欄數值校驗碼失敗", "table", table, "error", numRes.pErr)
	} else if numRes.mysql != nil {
		diag.Columns = append(diag.Columns, comparePerColumnChecksums(numRes.mysql, numRes.pg, "numeric")...)
	}

	if textRes.mErr != nil {
		m.logger.Warn("MySQL 逐欄文字校驗碼失敗", "table", table, "error", textRes.mErr)
	} else if textRes.pErr != nil {
		m.logger.Warn("PG 逐欄文字校驗碼失敗", "table", table, "error", textRes.pErr)
	} else if textRes.mysql != nil {
		diag.Columns = append(diag.Columns, comparePerColumnChecksums(textRes.mysql, textRes.pg, "text")...)
	}

	if dateRes.mErr != nil {
		m.logger.Warn("MySQL 逐欄日期校驗碼失敗", "table", table, "error", dateRes.mErr)
	} else if dateRes.pErr != nil {
		m.logger.Warn("PG 逐欄日期校驗碼失敗", "table", table, "error", dateRes.pErr)
	} else if dateRes.mysql != nil {
		diag.Columns = append(diag.Columns, comparePerColumnChecksums(dateRes.mysql, dateRes.pg, "date")...)
	}

	if len(diag.Columns) == 0 {
		return diag, nil
	}

	// Phase 2: PK detection
	pkCols, err := getPrimaryKeyColumns(ctx, mysqlDB, dbName, table)
	if err != nil {
		m.logger.Warn("PK 偵測失敗", "table", table, "error", err)
		diag.NoPK = true
		return diag, nil
	}
	if len(pkCols) == 0 {
		diag.NoPK = true
		return diag, nil
	}
	diag.PKColumns = pkCols

	// Phase 3: per-row diff
	mismatchCols := make([]string, 0, len(diag.Columns))
	for _, cm := range diag.Columns {
		mismatchCols = append(mismatchCols, cm.Column)
	}

	limit := m.cfg.DiagnoseLimit
	if limit <= 0 {
		limit = 10
	}

	rowDiffs, totalDiffs, err := diagnoseRowDiffs(ctx, mysqlDB, pgPool, table, pkCols, mismatchCols, limit)
	if err != nil {
		m.logger.Warn("逐行差異比對失敗", "table", table, "error", err)
		return diag, nil
	}

	diag.RowDiffs = rowDiffs
	diag.TotalDiffs = totalDiffs

	return diag, nil
}
