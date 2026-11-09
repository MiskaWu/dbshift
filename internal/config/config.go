package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config holds all migration configuration.
type Config struct {
	MySQL         DBConfig
	PG            DBConfig
	Databases     []string
	BatchSize     int
	Workers       int
	DiagnoseLimit int
	SkipDiagnosis bool
}

// DatabaseMapping maps a MySQL database name to a PostgreSQL database name.
type DatabaseMapping struct {
	MySQL string
	PG    string
}

// ParseDatabaseMappings parses database strings in "mysql_db:pg_db" format.
// If no colon is present, the same name is used for both.
func ParseDatabaseMappings(databases []string) []DatabaseMapping {
	mappings := make([]DatabaseMapping, len(databases))
	for i, db := range databases {
		if before, after, ok := strings.Cut(db, ":"); ok {
			mappings[i] = DatabaseMapping{MySQL: before, PG: after}
		} else {
			mappings[i] = DatabaseMapping{MySQL: db, PG: db}
		}
	}
	return mappings
}

// DBConfig holds connection parameters for a database.
type DBConfig struct {
	Host    string
	Port    int
	User    string
	Pass    string
	SSLMode string // PostgreSQL only; defaults to "disable" if empty
}

// MySQLDSN returns a go-sql-driver/mysql compatible DSN. The session time zone
// is fixed to UTC because loc=UTC only tells the driver how to label what it
// reads: the server still renders TIMESTAMP and evaluates
// UNIX_TIMESTAMP(DATETIME) in the session zone.
func (c DBConfig) MySQLDSN(database string) string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&loc=UTC&time_zone=%%27%%2B00%%3A00%%27",
		c.User, c.Pass, c.Host, c.Port, database)
}

// PostgresDSN returns a pgx compatible connection string.
func (c DBConfig) PostgresDSN(database string) string {
	sslMode := c.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.User, c.Pass),
		Host:     fmt.Sprintf("%s:%d", c.Host, c.Port),
		Path:     database,
		RawQuery: "sslmode=" + sslMode,
	}
	return u.String()
}

// EnvOr returns the value of the environment variable key, or fallback if unset.
func EnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// EnvOrInt returns the integer value of the environment variable key, or fallback if unset or invalid.
func EnvOrInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
