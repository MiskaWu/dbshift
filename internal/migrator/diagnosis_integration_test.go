package migrator

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// What each driver hands back for a date column, and what each server's time
// zone does to it, only shows against real servers. These tests use the same
// two servers as fk_integration_test.go and are skipped without them. One test
// changes MySQL's global time_zone while it runs, so use throwaway servers only.

type rowDiffLine struct {
	pk, column, mysql, pg string
}

func rowDiffLines(d *DiagnosisResult) []rowDiffLine {
	lines := make([]rowDiffLine, 0, len(d.RowDiffs))
	for _, rd := range d.RowDiffs {
		lines = append(lines, rowDiffLine{
			pk:     formatPK(rd.PK, d.PKColumns),
			column: rd.Column,
			mysql:  nullStr(rd.MySQLValue),
			pg:     nullStr(rd.PGValue),
		})
	}
	slices.SortFunc(lines, func(a, b rowDiffLine) int {
		return strings.Compare(a.pk+"\x00"+a.column, b.pk+"\x00"+b.column)
	})
	return lines
}

// diagnosedTable verifies the whole test database and returns the diagnosis of
// one table, which must have come out as a checksum mismatch.
func diagnosedTable(t *testing.T, env *fkEnv, table string) *DiagnosisResult {
	t.Helper()
	result, err := env.migrator.verifyDatabase(context.Background(), env.mapping)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, tr := range result.Tables {
		if tr.Table != table {
			continue
		}
		if tr.Status != "CHECKSUM_MISMATCH" {
			t.Fatalf("%s status = %s, want CHECKSUM_MISMATCH", table, tr.Status)
		}
		if tr.Diagnosis == nil {
			t.Fatalf("%s has no diagnosis", table)
		}
		return tr.Diagnosis
	}
	t.Fatalf("%s missing from verify result", table)
	return nil
}

func assertRowDiffs(t *testing.T, d *DiagnosisResult, want []rowDiffLine) {
	t.Helper()
	got := rowDiffLines(d)
	if !slices.Equal(got, want) {
		t.Errorf("row diffs:\n got %+v\nwant %+v", got, want)
	}
	if d.TotalDiffs != len(want) {
		t.Errorf("TotalDiffs = %d, want %d", d.TotalDiffs, len(want))
	}
}

func mismatchedColumns(d *DiagnosisResult) []string {
	cols := make([]string, 0, len(d.Columns))
	for _, c := range d.Columns {
		cols = append(cols, c.Column)
	}
	slices.Sort(cols)
	return cols
}

// A migrated table with every date column type. Only row 4 is changed on the
// target, so any other row showing up means the two sides were formatted
// differently: fractions like .120, NULLs and whole seconds are all in there.
func TestRowDiffsListDateColumns(t *testing.T) {
	env := newFKEnv(t, 2)
	mustExecSQL(t, env.mysql, `CREATE TABLE events (
		id INT PRIMARY KEY,
		day DATE NULL,
		at_s DATETIME NULL,
		at_ms DATETIME(3) NULL,
		at_us DATETIME(6) NULL,
		at_tz DATETIME(6) NULL,
		active TINYINT(1) NULL)`)
	mustExecPG(t, env.pg, `CREATE TABLE events (
		id INT PRIMARY KEY,
		day DATE,
		at_s TIMESTAMP,
		at_ms TIMESTAMP(3),
		at_us TIMESTAMP(6),
		at_tz TIMESTAMPTZ(6),
		active BOOLEAN)`)
	mustExecSQL(t, env.mysql, `INSERT INTO events VALUES
		(1, '2024-01-05', '2024-01-05 08:00:00', '2024-01-05 08:00:00.000', '2024-01-05 08:00:00.000000', '2024-01-05 08:00:00.100000', 0),
		(2, NULL, NULL, NULL, NULL, NULL, NULL),
		(3, '2024-02-06', '2024-02-06 09:10:11', '2024-02-06 09:10:11.100', '2024-02-06 09:10:11.120300', '2024-02-06 09:10:11.000000', 1),
		(4, '2024-03-09', '2024-03-09 10:20:30', '2024-03-09 10:20:30.120', '2024-03-09 10:20:30.123456', '2024-03-09 10:20:30.500000', 1)`)

	if err := env.migrator.migrateDatabase(context.Background(), env.mapping); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mustExecPG(t, env.pg, `UPDATE events SET
		day = day + 1,
		at_s = at_s + interval '1 second',
		at_ms = at_ms + interval '1 second',
		at_us = at_us + interval '1 second',
		at_tz = at_tz + interval '1 second',
		active = false
		WHERE id = 4`)

	assertRowDiffs(t, diagnosedTable(t, env, "events"), []rowDiffLine{
		{"id=4", "active", "1", "0"},
		{"id=4", "at_ms", "2024-03-09T10:20:30.12Z", "2024-03-09T10:20:31.12Z"},
		{"id=4", "at_s", "2024-03-09T10:20:30Z", "2024-03-09T10:20:31Z"},
		{"id=4", "at_tz", "2024-03-09T10:20:30.5Z", "2024-03-09T10:20:31.5Z"},
		{"id=4", "at_us", "2024-03-09T10:20:30.123456Z", "2024-03-09T10:20:31.123456Z"},
		{"id=4", "day", "2024-03-09T00:00:00Z", "2024-03-10T00:00:00Z"},
	})
}

// Neither server runs in UTC here. MySQL renders TIMESTAMP in its session zone
// and evaluates UNIX_TIMESTAMP(DATETIME) in it too, so without a fixed session
// zone the untouched column is flagged and row 1 shows up as a false diff.
func TestRowDiffsDoNotDependOnServerTimeZones(t *testing.T) {
	env := newFKEnv(t, 2)

	var previous string
	if err := env.mysql.QueryRow("SELECT @@global.time_zone").Scan(&previous); err != nil {
		t.Fatal(err)
	}
	mustExecSQL(t, env.mysql, "SET GLOBAL time_zone = '+08:00'")
	t.Cleanup(func() {
		if _, err := env.mysql.Exec("SET GLOBAL time_zone = '" + previous + "'"); err != nil {
			t.Errorf("restore time_zone: %v", err)
		}
	})
	mustExecPG(t, env.pg, "ALTER DATABASE "+fkTestDB+" SET timezone = 'America/New_York'")

	mustExecSQL(t, env.mysql, `CREATE TABLE clock (
		id INT PRIMARY KEY,
		wall DATETIME(3) NULL,
		instant TIMESTAMP(3) NULL,
		untouched DATETIME NULL)`)
	mustExecPG(t, env.pg, `CREATE TABLE clock (
		id INT PRIMARY KEY,
		wall TIMESTAMP(3),
		instant TIMESTAMPTZ(3),
		untouched TIMESTAMP)`)

	// The epoch literals keep both sides on the same instant in any session zone.
	mustExecSQL(t, env.mysql, `INSERT INTO clock VALUES
		(1, '2024-01-05 08:00:00.500', FROM_UNIXTIME(1704441600.500), '2024-01-05 08:00:00'),
		(2, '2024-03-09 10:20:30.120', FROM_UNIXTIME(1709979630.120), '2024-03-09 10:20:30')`)
	mustExecPG(t, env.pg, `INSERT INTO clock VALUES
		(1, '2024-01-05 08:00:00.500', to_timestamp(1704441600.500), '2024-01-05 08:00:00'),
		(2, '2024-03-09 10:20:31.120', to_timestamp(1709979631.120), '2024-03-09 10:20:30')`)

	d := diagnosedTable(t, env, "clock")
	if got, want := mismatchedColumns(d), []string{"instant", "wall"}; !slices.Equal(got, want) {
		t.Errorf("mismatched columns = %v, want %v", got, want)
	}
	assertRowDiffs(t, d, []rowDiffLine{
		{"id=2", "instant", "2024-03-09T10:20:30.12Z", "2024-03-09T10:20:31.12Z"},
		{"id=2", "wall", "2024-03-09T10:20:30.12Z", "2024-03-09T10:20:31.12Z"},
	})
}

// Rows are paired by key, so a key the two sides format differently leaves every
// row unmatched. The non-date keys pin down what already worked before dates did.
func TestRowDiffsPairRowsByKeyOfAnyType(t *testing.T) {
	tests := []struct {
		name          string
		mysqlType     string
		pgType        string
		first, second string
		wantKey       string
	}{
		{"datetime with fraction", "DATETIME(3)", "TIMESTAMP(3)", "2024-03-09 10:20:30.500", "2024-03-09 10:20:31.000", "2024-03-09T10:20:30.5Z"},
		{"date", "DATE", "DATE", "2024-03-09", "2024-03-10", "2024-03-09T00:00:00Z"},
		{"decimal keeps its scale", "DECIMAL(10,2)", "NUMERIC(10,2)", "1.50", "2.00", "1.50"},
		{"bigint", "BIGINT", "BIGINT", "9007199254740993", "1", "9007199254740993"},
		{"varchar", "VARCHAR(20)", "VARCHAR(20)", "a", "b", "a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newFKEnv(t, 2)
			mustExecSQL(t, env.mysql, fmt.Sprintf("CREATE TABLE readings (k %s PRIMARY KEY, val INT)", tt.mysqlType))
			mustExecPG(t, env.pg, fmt.Sprintf("CREATE TABLE readings (k %s PRIMARY KEY, val INT)", tt.pgType))
			mustExecSQL(t, env.mysql, fmt.Sprintf("INSERT INTO readings VALUES ('%s', 10), ('%s', 20)", tt.first, tt.second))
			mustExecPG(t, env.pg, fmt.Sprintf("INSERT INTO readings VALUES ('%s', 11), ('%s', 20)", tt.first, tt.second))

			assertRowDiffs(t, diagnosedTable(t, env, "readings"), []rowDiffLine{
				{"k=" + tt.wantKey, "val", "10", "11"},
			})
		})
	}
}
