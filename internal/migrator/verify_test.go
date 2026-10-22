package migrator

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func boolPtr(b bool) *bool { return &b }

func TestHasMismatch(t *testing.T) {
	tests := []struct {
		name    string
		results []VerifyResult
		want    bool
	}{
		{
			name:    "empty results",
			results: nil,
			want:    false,
		},
		{
			name: "all OK",
			results: []VerifyResult{{
				Database: "test",
				Tables: []TableResult{
					{Table: "t1", Status: "OK"},
					{Table: "t2", Status: "OK"},
				},
			}},
			want: false,
		},
		{
			name: "count mismatch",
			results: []VerifyResult{{
				Database: "test",
				Tables: []TableResult{
					{Table: "t1", Status: "OK"},
					{Table: "t2", Status: "COUNT_MISMATCH"},
				},
			}},
			want: true,
		},
		{
			name: "checksum mismatch",
			results: []VerifyResult{{
				Database: "test",
				Tables: []TableResult{
					{Table: "t1", Status: "CHECKSUM_MISMATCH"},
				},
			}},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasMismatch(tt.results); got != tt.want {
				t.Errorf("HasMismatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestComputeStatus(t *testing.T) {
	tests := []struct {
		name          string
		countMatch    bool
		checksumMatch *bool
		want          string
	}{
		{"count mismatch", false, nil, "COUNT_MISMATCH"},
		{"count match no checksum", true, nil, "OK"},
		{"count match checksum match", true, boolPtr(true), "OK"},
		{"count match checksum mismatch", true, boolPtr(false), "CHECKSUM_MISMATCH"},
		{"count mismatch overrides checksum", false, boolPtr(true), "COUNT_MISMATCH"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := TableResult{
				CountMatch:    tt.countMatch,
				ChecksumMatch: tt.checksumMatch,
			}
			got := computeStatus(tr)
			if got != tt.want {
				t.Errorf("computeStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPrintVerifyResults(t *testing.T) {
	results := []VerifyResult{
		{
			Database: "testdb",
			Tables: []TableResult{
				{
					Table:         "users",
					MySQLCount:    100,
					PGCount:       100,
					CountMatch:    true,
					ChecksumMatch: boolPtr(true),
					Status:        "OK",
				},
				{
					Table:         "orders",
					MySQLCount:    50,
					PGCount:       48,
					CountMatch:    false,
					ChecksumMatch: nil,
					Status:        "COUNT_MISMATCH",
				},
			},
		},
	}

	var buf bytes.Buffer
	PrintVerifyResults(&buf, results)
	output := buf.String()

	// Verify key elements are present
	if !strings.Contains(output, "testdb") {
		t.Error("output should contain database name")
	}
	if !strings.Contains(output, "users") {
		t.Error("output should contain table name 'users'")
	}
	if !strings.Contains(output, "orders") {
		t.Error("output should contain table name 'orders'")
	}
	if !strings.Contains(output, "100") {
		t.Error("output should contain count 100")
	}
	if !strings.Contains(output, "MATCH") {
		t.Error("output should contain MATCH for checksum")
	}
	if !strings.Contains(output, "COUNT_MISMATCH") {
		t.Error("output should contain COUNT_MISMATCH status")
	}
	if !strings.Contains(output, "WARNING") {
		t.Error("output should contain WARNING for mismatch")
	}
	// Ensure no CJK characters leaked into table output
	for _, zh := range []string{"資料表", "校驗碼", "一致", "警告"} {
		if strings.Contains(output, zh) {
			t.Errorf("output should not contain CJK string %q in table output", zh)
		}
	}
}

func TestConvertMySQLValue(t *testing.T) {
	tests := []struct {
		name      string
		value     interface{}
		mysqlType string
		pgType    string
		wantType  string // "string", "[]byte", "bool", "nil", "passthrough", "time"
	}{
		{"nil value", nil, "VARCHAR", "", "nil"},
		{"text as bytes", []byte("hello"), "VARCHAR", "", "string"},
		{"text column", []byte("world"), "TEXT", "", "string"},
		{"json column", []byte(`{"a":1}`), "JSON", "", "string"},
		{"decimal column", []byte("123.45"), "DECIMAL", "", "string"},
		{"blob stays bytes", []byte{0x00, 0xFF}, "BLOB", "", "[]byte"},
		{"binary stays bytes", []byte{0x01}, "BINARY", "", "[]byte"},
		{"int64 passthrough", int64(42), "INT", "", "passthrough"},
		{"float64 passthrough", float64(3.14), "DOUBLE", "", "passthrough"},
		{"bit single byte", []byte{1}, "BIT", "", "bool"},
		{"bit zero", []byte{0}, "BIT", "", "bool"},
		// TINYINT → PG boolean
		{"tinyint 0 to bool false", int64(0), "TINYINT", "boolean", "bool"},
		{"tinyint 1 to bool true", int64(1), "TINYINT", "boolean", "bool"},
		{"tinyint 2 to bool true", int64(2), "TINYINT", "boolean", "bool"},
		{"nil tinyint to bool nil", nil, "TINYINT", "boolean", "nil"},
		{"tinyint bytes to bool", []byte("1"), "TINYINT", "boolean", "bool"},
		{"tinyint without pg bool", int64(1), "TINYINT", "smallint", "passthrough"},
		// MySQL TIME → PG time
		{"time to pg time", []byte("14:30:00"), "TIME", "time without time zone", "time"},
		{"time with frac to pg time", []byte("14:30:00.123456"), "TIME", "time without time zone", "time"},
		{"nil time", nil, "TIME", "time without time zone", "nil"},
		{"time to pg timetz", []byte("09:00:00"), "TIME", "time with time zone", "time"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertMySQLValue(tt.value, tt.mysqlType, tt.pgType)

			switch tt.wantType {
			case "nil":
				if got != nil {
					t.Errorf("expected nil, got %v (%T)", got, got)
				}
			case "string":
				if _, ok := got.(string); !ok {
					t.Errorf("expected string, got %T", got)
				}
			case "[]byte":
				if _, ok := got.([]byte); !ok {
					t.Errorf("expected []byte, got %T", got)
				}
			case "bool":
				if _, ok := got.(bool); !ok {
					t.Errorf("expected bool, got %T", got)
				}
			case "time":
				if _, ok := got.(time.Time); !ok {
					t.Errorf("expected time.Time, got %T", got)
				}
			case "passthrough":
				if got != tt.value {
					t.Errorf("expected passthrough %v, got %v", tt.value, got)
				}
			}
		})
	}
}

func TestFilterExistingCols(t *testing.T) {
	tests := []struct {
		name   string
		cols   []string
		pgCols map[string]string
		want   []string
	}{
		{
			name:   "all columns exist",
			cols:   []string{"id", "name", "score"},
			pgCols: map[string]string{"id": "integer", "name": "text", "score": "bigint"},
			want:   []string{"id", "name", "score"},
		},
		{
			name:   "some columns missing in PG",
			cols:   []string{"id", "first_open", "name"},
			pgCols: map[string]string{"id": "integer", "name": "text"},
			want:   []string{"id", "name"},
		},
		{
			name:   "all columns missing in PG",
			cols:   []string{"old_a", "old_b"},
			pgCols: map[string]string{"id": "integer"},
			want:   nil,
		},
		{
			name:   "empty input",
			cols:   []string{},
			pgCols: map[string]string{"id": "integer"},
			want:   nil,
		},
		{
			name:   "empty PG map",
			cols:   []string{"id", "name"},
			pgCols: map[string]string{},
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterExistingCols(tt.cols, tt.pgCols)
			if len(got) != len(tt.want) {
				t.Fatalf("filterExistingCols() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("filterExistingCols()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCombineChecksumResults(t *testing.T) {
	tests := []struct {
		name  string
		parts []bool
		want  *bool
	}{
		{"empty", nil, nil},
		{"single true", []bool{true}, boolPtr(true)},
		{"single false", []bool{false}, boolPtr(false)},
		{"all true", []bool{true, true, true}, boolPtr(true)},
		{"one false", []bool{true, false, true}, boolPtr(false)},
		{"all false", []bool{false, false}, boolPtr(false)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := combineChecksumResults(tt.parts)
			if tt.want == nil {
				if got != nil {
					t.Errorf("combineChecksumResults() = %v, want nil", *got)
				}
			} else if got == nil {
				t.Errorf("combineChecksumResults() = nil, want %v", *tt.want)
			} else if *got != *tt.want {
				t.Errorf("combineChecksumResults() = %v, want %v", *got, *tt.want)
			}
		})
	}
}

func TestToBool(t *testing.T) {
	tests := []struct {
		name string
		v    interface{}
		want bool
	}{
		{"int64 0", int64(0), false},
		{"int64 1", int64(1), true},
		{"int64 -1", int64(-1), true},
		{"float64 0", float64(0), false},
		{"float64 1.5", float64(1.5), true},
		{"bytes 1", []byte("1"), true},
		{"bytes 0", []byte("0"), false},
		{"string 1", "1", true},
		{"string 0", "0", false},
		{"string empty", "", false},
		{"bool true", true, true},
		{"bool false", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toBool(tt.v).(bool)
			if !ok {
				t.Fatalf("toBool(%v) did not return bool", tt.v)
			}
			if got != tt.want {
				t.Errorf("toBool(%v) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}
