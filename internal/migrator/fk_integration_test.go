package migrator

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/miskawu/dbshift/internal/config"
)

// Foreign keys and TRUNCATE are PostgreSQL's behaviour, so a fake would prove
// nothing here. These tests need real servers and are skipped otherwise:
//
//	DBSHIFT_TEST_MYSQL_URL=mysql://root:test@127.0.0.1:33306
//	DBSHIFT_TEST_PG_URL=postgres://postgres:test@127.0.0.1:35433
//
// Both servers get a database named dbshift_fk_test, dropped on every run.
const fkTestDB = "dbshift_fk_test"

func dbConfigFromEnv(t *testing.T, key string) config.DBConfig {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		t.Skipf("%s not set; skipping integration test", key)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", key, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("%s needs a port: %v", key, err)
	}
	pass, _ := u.User.Password()
	return config.DBConfig{Host: u.Hostname(), Port: port, User: u.User.Username(), Pass: pass}
}

type fkEnv struct {
	migrator *Migrator
	mapping  config.DatabaseMapping
	mysql    *sql.DB
	pg       *pgxpool.Pool
}

// newFKEnv gives both servers an empty test database and the same two tables.
// a_child sorts before z_parent, so with one worker the child is migrated first.
func newFKEnv(t *testing.T, workers int) *fkEnv {
	t.Helper()
	ctx := context.Background()
	my, pg := dbConfigFromEnv(t, "DBSHIFT_TEST_MYSQL_URL"), dbConfigFromEnv(t, "DBSHIFT_TEST_PG_URL")

	admin, err := sql.Open("mysql", my.MySQLDSN(""))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	mustExecSQL(t, admin, "DROP DATABASE IF EXISTS "+fkTestDB)
	mustExecSQL(t, admin, "CREATE DATABASE "+fkTestDB)

	pgAdmin, err := pgxpool.New(ctx, pg.PostgresDSN("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer pgAdmin.Close()
	mustExecPG(t, pgAdmin, "DROP DATABASE IF EXISTS "+fkTestDB+" WITH (FORCE)")
	mustExecPG(t, pgAdmin, "CREATE DATABASE "+fkTestDB)

	source, err := sql.Open("mysql", my.MySQLDSN(fkTestDB))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	target, err := pgxpool.New(ctx, pg.PostgresDSN(fkTestDB))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(target.Close)

	mustExecSQL(t, source, "CREATE TABLE z_parent (id INT PRIMARY KEY)")
	mustExecSQL(t, source, "CREATE TABLE a_child (id INT PRIMARY KEY, parent_id INT NOT NULL)")
	mustExecSQL(t, source, "INSERT INTO z_parent VALUES (1), (2)")
	mustExecSQL(t, source, "INSERT INTO a_child VALUES (10, 1), (11, 1), (12, 2)")

	mustExecPG(t, target, "CREATE TABLE z_parent (id INT PRIMARY KEY)")
	mustExecPG(t, target, "CREATE TABLE a_child (id INT PRIMARY KEY, parent_id INT NOT NULL REFERENCES z_parent(id))")

	cfg := &config.Config{MySQL: my, PG: pg, BatchSize: 100, Workers: workers}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &fkEnv{
		migrator: NewMigrator(cfg, logger),
		mapping:  config.DatabaseMapping{MySQL: fkTestDB, PG: fkTestDB},
		mysql:    source,
		pg:       target,
	}
}

func mustExecSQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("mysql %q: %v", query, err)
	}
}

func mustExecPG(t *testing.T, pool *pgxpool.Pool, query string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), query); err != nil {
		t.Fatalf("pg %q: %v", query, err)
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestParentMigratedAfterChildKeepsChildRows(t *testing.T) {
	for _, workers := range []int{1, 4} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			env := newFKEnv(t, workers)
			// Old rows on the target must be replaced, not kept.
			mustExecPG(t, env.pg, "INSERT INTO z_parent VALUES (99)")
			mustExecPG(t, env.pg, "INSERT INTO a_child VALUES (98, 99)")

			if err := env.migrator.migrateDatabase(context.Background(), env.mapping); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if got := countRows(t, env.pg, "a_child"); got != 3 {
				t.Errorf("a_child rows = %d, want 3: the parent's truncate must not wipe the child", got)
			}
			if got := countRows(t, env.pg, "z_parent"); got != 2 {
				t.Errorf("z_parent rows = %d, want 2", got)
			}
		})
	}
}

func TestTableOutsideMigrationStopsBeforeAnyChange(t *testing.T) {
	env := newFKEnv(t, 2)
	// audit_log exists only on the target and points at a migrated table.
	mustExecPG(t, env.pg, "CREATE TABLE audit_log (id INT PRIMARY KEY, parent_id INT REFERENCES z_parent(id))")
	mustExecPG(t, env.pg, "INSERT INTO z_parent VALUES (99)")
	mustExecPG(t, env.pg, "INSERT INTO a_child VALUES (98, 99)")
	mustExecPG(t, env.pg, "INSERT INTO audit_log VALUES (1, 99)")

	if err := env.migrator.migrateDatabase(context.Background(), env.mapping); err == nil {
		t.Fatal("migrate succeeded; a table outside the migration should stop it")
	}
	for table, want := range map[string]int{"audit_log": 1, "z_parent": 1, "a_child": 1} {
		if got := countRows(t, env.pg, table); got != want {
			t.Errorf("%s rows = %d, want %d: a refused run must not change data", table, got, want)
		}
	}
}
