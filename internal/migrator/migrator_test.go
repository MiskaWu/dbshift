package migrator

import "testing"

func TestFilterColumns(t *testing.T) {
	tests := []struct {
		name            string
		mysqlColNames   []string
		pgColTypeMap    map[string]string
		wantKeepCount   int
		wantSkippedCols []string
	}{
		{
			name:            "all columns exist in PG",
			mysqlColNames:   []string{"id", "name", "email"},
			pgColTypeMap:    map[string]string{"id": "integer", "name": "text", "email": "text"},
			wantKeepCount:   3,
			wantSkippedCols: nil,
		},
		{
			name:            "one column missing in PG",
			mysqlColNames:   []string{"id", "name", "legacy_field", "email"},
			pgColTypeMap:    map[string]string{"id": "integer", "name": "text", "email": "text"},
			wantKeepCount:   3,
			wantSkippedCols: []string{"legacy_field"},
		},
		{
			name:            "multiple columns missing in PG",
			mysqlColNames:   []string{"id", "old_col1", "name", "old_col2"},
			pgColTypeMap:    map[string]string{"id": "integer", "name": "text"},
			wantKeepCount:   2,
			wantSkippedCols: []string{"old_col1", "old_col2"},
		},
		{
			name:            "all columns missing in PG",
			mysqlColNames:   []string{"old_a", "old_b"},
			pgColTypeMap:    map[string]string{"id": "integer"},
			wantKeepCount:   0,
			wantSkippedCols: []string{"old_a", "old_b"},
		},
		{
			name:            "empty MySQL columns",
			mysqlColNames:   []string{},
			pgColTypeMap:    map[string]string{"id": "integer"},
			wantKeepCount:   0,
			wantSkippedCols: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keepIndices, skippedCols := filterColumns(tt.mysqlColNames, tt.pgColTypeMap)

			if len(keepIndices) != tt.wantKeepCount {
				t.Errorf("keepIndices count = %d, want %d", len(keepIndices), tt.wantKeepCount)
			}

			if len(skippedCols) != len(tt.wantSkippedCols) {
				t.Errorf("skippedCols = %v, want %v", skippedCols, tt.wantSkippedCols)
			} else {
				for i, name := range skippedCols {
					if name != tt.wantSkippedCols[i] {
						t.Errorf("skippedCols[%d] = %q, want %q", i, name, tt.wantSkippedCols[i])
					}
				}
			}

			// Verify keepIndices point to correct columns
			for _, idx := range keepIndices {
				colName := tt.mysqlColNames[idx]
				if _, ok := tt.pgColTypeMap[colName]; !ok {
					t.Errorf("keepIndices contains index %d for column %q which is not in PG", idx, colName)
				}
			}
		})
	}
}
