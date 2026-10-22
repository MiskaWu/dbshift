package config

import (
	"os"
	"testing"
)

func TestDBConfig_MySQLDSN(t *testing.T) {
	tests := []struct {
		name     string
		cfg      DBConfig
		database string
		want     string
	}{
		{
			name:     "standard connection",
			cfg:      DBConfig{Host: "localhost", Port: 3306, User: "root", Pass: "secret"},
			database: "testdb",
			want:     "root:secret@tcp(localhost:3306)/testdb?charset=utf8mb4&parseTime=true&loc=UTC",
		},
		{
			name:     "empty password",
			cfg:      DBConfig{Host: "10.0.0.1", Port: 4000, User: "admin", Pass: ""},
			database: "mydb",
			want:     "admin:@tcp(10.0.0.1:4000)/mydb?charset=utf8mb4&parseTime=true&loc=UTC",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.MySQLDSN(tt.database)
			if got != tt.want {
				t.Errorf("MySQLDSN() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDBConfig_PostgresDSN(t *testing.T) {
	tests := []struct {
		name     string
		cfg      DBConfig
		database string
		want     string
	}{
		{
			name:     "standard connection",
			cfg:      DBConfig{Host: "localhost", Port: 5432, User: "pg", Pass: "pass", SSLMode: "disable"},
			database: "mydb",
			want:     "postgres://pg:pass@localhost:5432/mydb?sslmode=disable",
		},
		{
			name:     "password with special characters is URL-encoded",
			cfg:      DBConfig{Host: "pg.example.com", Port: 5432, User: "postgres", Pass: "p~ss:w@rd/x y", SSLMode: "disable"},
			database: "mydb",
			want:     "postgres://postgres:p~ss%3Aw%40rd%2Fx%20y@pg.example.com:5432/mydb?sslmode=disable",
		},
		{
			name:     "ssl mode require",
			cfg:      DBConfig{Host: "pg.example.com", Port: 5432, User: "pg", Pass: "pass", SSLMode: "require"},
			database: "prod",
			want:     "postgres://pg:pass@pg.example.com:5432/prod?sslmode=require",
		},
		{
			name:     "ssl mode defaults to disable when empty",
			cfg:      DBConfig{Host: "localhost", Port: 5432, User: "pg", Pass: "pass"},
			database: "mydb",
			want:     "postgres://pg:pass@localhost:5432/mydb?sslmode=disable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.PostgresDSN(tt.database)
			if got != tt.want {
				t.Errorf("PostgresDSN() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseDatabaseMappings(t *testing.T) {
	tests := []struct {
		name      string
		databases []string
		want      []DatabaseMapping
	}{
		{
			name:      "same name for both",
			databases: []string{"mydb"},
			want:      []DatabaseMapping{{MySQL: "mydb", PG: "mydb"}},
		},
		{
			name:      "different source and target",
			databases: []string{"app:app_stage"},
			want:      []DatabaseMapping{{MySQL: "app", PG: "app_stage"}},
		},
		{
			name:      "mixed",
			databases: []string{"db1", "src_db:dst_db", "db3"},
			want: []DatabaseMapping{
				{MySQL: "db1", PG: "db1"},
				{MySQL: "src_db", PG: "dst_db"},
				{MySQL: "db3", PG: "db3"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseDatabaseMappings(tt.databases)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseDatabaseMappings() returned %d items, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestEnvOr(t *testing.T) {
	t.Run("returns env var when set", func(t *testing.T) {
		t.Setenv("TEST_DBSHIFT_ENV", "from_env")
		if got := EnvOr("TEST_DBSHIFT_ENV", "fallback"); got != "from_env" {
			t.Errorf("EnvOr() = %q, want %q", got, "from_env")
		}
	})

	t.Run("returns fallback when not set", func(t *testing.T) {
		os.Unsetenv("TEST_DBSHIFT_MISSING")
		if got := EnvOr("TEST_DBSHIFT_MISSING", "fallback"); got != "fallback" {
			t.Errorf("EnvOr() = %q, want %q", got, "fallback")
		}
	})
}

func TestEnvOrInt(t *testing.T) {
	t.Run("returns parsed int from env", func(t *testing.T) {
		t.Setenv("TEST_DBSHIFT_INT", "42")
		if got := EnvOrInt("TEST_DBSHIFT_INT", 0); got != 42 {
			t.Errorf("EnvOrInt() = %d, want %d", got, 42)
		}
	})

	t.Run("returns fallback for invalid int", func(t *testing.T) {
		t.Setenv("TEST_DBSHIFT_INT_BAD", "abc")
		if got := EnvOrInt("TEST_DBSHIFT_INT_BAD", 99); got != 99 {
			t.Errorf("EnvOrInt() = %d, want %d", got, 99)
		}
	})

	t.Run("returns fallback when not set", func(t *testing.T) {
		os.Unsetenv("TEST_DBSHIFT_INT_NONE")
		if got := EnvOrInt("TEST_DBSHIFT_INT_NONE", 99); got != 99 {
			t.Errorf("EnvOrInt() = %d, want %d", got, 99)
		}
	})
}
