package migrator

import (
	"bytes"
	"strings"
	"testing"
)

func TestComparePerColumnChecksums(t *testing.T) {
	tests := []struct {
		name     string
		mysql    map[string]string
		pg       map[string]string
		category string
		want     []ColumnMismatch
	}{
		{
			name:     "all match",
			mysql:    map[string]string{"id": "100", "score": "250"},
			pg:       map[string]string{"id": "100", "score": "250"},
			category: "numeric",
			want:     nil,
		},
		{
			name:     "one mismatch",
			mysql:    map[string]string{"id": "100", "score": "250"},
			pg:       map[string]string{"id": "100", "score": "249"},
			category: "numeric",
			want: []ColumnMismatch{
				{Column: "score", Category: "numeric", MySQLValue: "250", PGValue: "249"},
			},
		},
		{
			name:     "multiple mismatches",
			mysql:    map[string]string{"a": "1", "b": "2", "c": "3"},
			pg:       map[string]string{"a": "1", "b": "9", "c": "8"},
			category: "text",
			want: []ColumnMismatch{
				{Column: "b", Category: "text", MySQLValue: "2", PGValue: "9"},
				{Column: "c", Category: "text", MySQLValue: "3", PGValue: "8"},
			},
		},
		{
			name:     "empty maps",
			mysql:    map[string]string{},
			pg:       map[string]string{},
			category: "numeric",
			want:     nil,
		},
		{
			name:     "column in mysql not in pg",
			mysql:    map[string]string{"id": "100", "extra": "50"},
			pg:       map[string]string{"id": "100"},
			category: "numeric",
			want: []ColumnMismatch{
				{Column: "extra", Category: "numeric", MySQLValue: "50", PGValue: "(missing)"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := comparePerColumnChecksums(tt.mysql, tt.pg, tt.category)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d mismatches, want %d\ngot:  %+v\nwant: %+v", len(got), len(tt.want), got, tt.want)
			}
			// Build a map for order-independent comparison
			wantMap := make(map[string]ColumnMismatch)
			for _, w := range tt.want {
				wantMap[w.Column] = w
			}
			for _, g := range got {
				w, ok := wantMap[g.Column]
				if !ok {
					t.Errorf("unexpected mismatch for column %q", g.Column)
					continue
				}
				if g != w {
					t.Errorf("column %q: got %+v, want %+v", g.Column, g, w)
				}
			}
		})
	}
}

func TestPrintDiagnosis(t *testing.T) {
	t.Run("with column mismatches and row diffs", func(t *testing.T) {
		diag := &DiagnosisResult{
			Columns: []ColumnMismatch{
				{Column: "score", Category: "numeric", MySQLValue: "250", PGValue: "249"},
			},
			PKColumns: []string{"id"},
			RowDiffs: []RowDiff{
				{
					PK:         map[string]string{"id": "42"},
					Column:     "score",
					MySQLValue: strPtr("95"),
					PGValue:    strPtr("94"),
				},
			},
			RowDiffCap: 10,
			TotalDiffs: 1,
		}
		var buf bytes.Buffer
		printDiagnosis(&buf, diag)
		out := buf.String()

		if !strings.Contains(out, "score") {
			t.Error("should contain column name 'score'")
		}
		if !strings.Contains(out, "250") {
			t.Error("should contain MySQL checksum value '250'")
		}
		if !strings.Contains(out, "249") {
			t.Error("should contain PG checksum value '249'")
		}
		if !strings.Contains(out, "id=42") {
			t.Error("should contain PK 'id=42'")
		}
		if !strings.Contains(out, "95") {
			t.Error("should contain MySQL row value '95'")
		}
		if !strings.Contains(out, "94") {
			t.Error("should contain PG row value '94'")
		}
	})

	t.Run("no PK found", func(t *testing.T) {
		diag := &DiagnosisResult{
			Columns: []ColumnMismatch{
				{Column: "name", Category: "text", MySQLValue: "1000", PGValue: "999"},
			},
			NoPK: true,
		}
		var buf bytes.Buffer
		printDiagnosis(&buf, diag)
		out := buf.String()

		if !strings.Contains(out, "name") {
			t.Error("should contain column name")
		}
		if !strings.Contains(out, "No primary key") && !strings.Contains(out, "no primary key") {
			t.Error("should mention no primary key found")
		}
	})

	t.Run("nil value in row diff", func(t *testing.T) {
		diag := &DiagnosisResult{
			Columns: []ColumnMismatch{
				{Column: "bio", Category: "text", MySQLValue: "500", PGValue: "0"},
			},
			PKColumns: []string{"id"},
			RowDiffs: []RowDiff{
				{
					PK:         map[string]string{"id": "1"},
					Column:     "bio",
					MySQLValue: strPtr("hello"),
					PGValue:    nil,
				},
			},
			RowDiffCap: 10,
			TotalDiffs: 1,
		}
		var buf bytes.Buffer
		printDiagnosis(&buf, diag)
		out := buf.String()

		if !strings.Contains(out, "NULL") {
			t.Error("should show NULL for nil PG value")
		}
	})

	t.Run("truncated row diffs", func(t *testing.T) {
		diag := &DiagnosisResult{
			Columns: []ColumnMismatch{
				{Column: "score", Category: "numeric", MySQLValue: "100", PGValue: "99"},
			},
			PKColumns: []string{"id"},
			RowDiffs: []RowDiff{
				{PK: map[string]string{"id": "1"}, Column: "score", MySQLValue: strPtr("10"), PGValue: strPtr("9")},
			},
			RowDiffCap: 10,
			TotalDiffs: 25,
		}
		var buf bytes.Buffer
		printDiagnosis(&buf, diag)
		out := buf.String()

		if !strings.Contains(out, "24") {
			t.Errorf("should mention remaining diff count (24 more), got: %s", out)
		}
	})
}

func strPtr(s string) *string { return &s }

func TestBuildMySQLPerColumnNumericQuery(t *testing.T) {
	tests := []struct {
		name  string
		table string
		cols  []string
		want  string
	}{
		{
			name:  "single column",
			table: "users",
			cols:  []string{"score"},
			want:  "SELECT CAST(COALESCE(SUM(`score`), 0) AS CHAR) AS `score` FROM `users`",
		},
		{
			name:  "multiple columns",
			table: "orders",
			cols:  []string{"id", "total"},
			want:  "SELECT CAST(COALESCE(SUM(`id`), 0) AS CHAR) AS `id`, CAST(COALESCE(SUM(`total`), 0) AS CHAR) AS `total` FROM `orders`",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildMySQLPerColumnNumericQuery(tt.table, tt.cols)
			if got != tt.want {
				t.Errorf("got:\n  %s\nwant:\n  %s", got, tt.want)
			}
		})
	}
}

func TestBuildPGPerColumnNumericQuery(t *testing.T) {
	tests := []struct {
		name    string
		table   string
		cols    []string
		pgTypes map[string]string
		wantSub []string // substrings that must appear
	}{
		{
			name:    "regular integer column",
			table:   "users",
			cols:    []string{"score"},
			pgTypes: map[string]string{"score": "bigint"},
			wantSub: []string{`SUM("score"::bigint)`, `FROM "users"`},
		},
		{
			name:    "boolean column uses ::int",
			table:   "flags",
			cols:    []string{"active"},
			pgTypes: map[string]string{"active": "boolean"},
			wantSub: []string{`SUM("active"::int)`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPGPerColumnNumericQuery(tt.table, tt.cols, tt.pgTypes)
			for _, sub := range tt.wantSub {
				if !strings.Contains(got, sub) {
					t.Errorf("query missing %q\ngot: %s", sub, got)
				}
			}
		})
	}
}

func TestBuildMySQLPerColumnTextQuery(t *testing.T) {
	got := buildMySQLPerColumnTextQuery("posts", []string{"title", "body"})
	want := "SELECT COALESCE(SUM(CHAR_LENGTH(`title`)), 0) AS `title`, COALESCE(SUM(CHAR_LENGTH(`body`)), 0) AS `body` FROM `posts`"
	if got != want {
		t.Errorf("got:\n  %s\nwant:\n  %s", got, want)
	}
}

func TestBuildPGPerColumnTextQuery(t *testing.T) {
	got := buildPGPerColumnTextQuery("posts", []string{"title", "body"})
	if !strings.Contains(got, `SUM(LENGTH("title"))`) {
		t.Errorf("missing LENGTH for title, got: %s", got)
	}
	if !strings.Contains(got, `FROM "posts"`) {
		t.Errorf("missing FROM clause, got: %s", got)
	}
}

func TestBuildMySQLPerColumnDateQuery(t *testing.T) {
	got := buildMySQLPerColumnDateQuery("events", []string{"created_at"})
	if !strings.Contains(got, "UNIX_TIMESTAMP(MIN(`created_at`))") {
		t.Errorf("missing MIN, got: %s", got)
	}
	if !strings.Contains(got, "UNIX_TIMESTAMP(MAX(`created_at`))") {
		t.Errorf("missing MAX, got: %s", got)
	}
}

func TestBuildPGPerColumnDateQuery(t *testing.T) {
	got := buildPGPerColumnDateQuery("events", []string{"created_at"})
	if !strings.Contains(got, `EXTRACT(EPOCH FROM MIN("created_at"))`) {
		t.Errorf("missing MIN epoch, got: %s", got)
	}
	if !strings.Contains(got, `EXTRACT(EPOCH FROM MAX("created_at"))`) {
		t.Errorf("missing MAX epoch, got: %s", got)
	}
}
