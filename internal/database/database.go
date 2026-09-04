// Package database opens the configured database backend (SQLite or
// Postgres) and applies embedded SQL migrations using Goose, tracked in a
// goose_db_version table. Store SQL is written once against the common
// subset: the Postgres driver rewrites ? placeholders (see postgres.go) and
// each dialect has its own migration files.
package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	"github.com/pressly/goose/v3"

	_ "modernc.org/sqlite"
)

//go:embed migrations
var migrationFS embed.FS

// NormalizeDriver maps a driver flag value (and its aliases) onto the
// canonical "sqlite" or "postgres".
func NormalizeDriver(driver string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "", "sqlite", "sqlite3":
		return "sqlite", nil
	case "postgres", "postgresql", "pg":
		return "postgres", nil
	default:
		return "", fmt.Errorf("unsupported db driver %q (want sqlite or postgres)", driver)
	}
}

// ResolveDriver picks the backend for a run: the driver flag when given,
// otherwise detected from the DSN — a postgres:// URL or a libpq key=value
// string means Postgres, anything else is a SQLite file path.
func ResolveDriver(driver, dsn string) (string, error) {
	if strings.TrimSpace(driver) == "" {
		if looksLikePostgresDSN(dsn) {
			return "postgres", nil
		}
		return "sqlite", nil
	}
	return NormalizeDriver(driver)
}

// looksLikePostgresDSN reports whether dsn is in a form only Postgres
// understands.
func looksLikePostgresDSN(dsn string) bool {
	low := strings.ToLower(dsn)
	if strings.HasPrefix(low, "postgres://") || strings.HasPrefix(low, "postgresql://") {
		return true
	}
	// Key/value form: host=… dbname=…
	return dsnRegexp.MatchString(dsn)
}

var dsnRegexp = regexp.MustCompile(`(?:^|\s)(?:host|dbname)=\S`)

// Open opens the database named by driver ("sqlite" or "postgres", as
// returned by NormalizeDriver) at dsn: a file path for SQLite (created if
// necessary, parent directory must exist) or a libpq-style connection string
// for Postgres.
func Open(driver, dsn string) (*sql.DB, error) {
	driver, err := NormalizeDriver(driver)
	if err != nil {
		return nil, err
	}
	var db *sql.DB
	switch driver {
	case "postgres":
		db, err = openPostgres(dsn)
	default: // sqlite
		name := "file:" + dsn + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
		db, err = sql.Open("sqlite", name)
		if err != nil {
			return nil, fmt.Errorf("open sqlite: %w", err)
		}
		// SQLite handles one writer at a time; serialise access through a
		// single connection to avoid SQLITE_BUSY churn entirely.
		db.SetMaxOpenConns(1)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s: %w", driver, err)
	}
	return db, nil
}

// Migrate applies all pending embedded migrations for the driver via Goose.
func Migrate(db *sql.DB, driver string) error {
	driver, err := NormalizeDriver(driver)
	if err != nil {
		return err
	}
	dialect := goose.DialectSQLite3
	dir := "migrations/sqlite"
	if driver == "postgres" {
		dialect = goose.DialectPostgres
		dir = "migrations/postgres"
	}
	fsys, err := fs.Sub(migrationFS, dir)
	if err != nil {
		return fmt.Errorf("mount migrations: %w", err)
	}
	provider, err := goose.NewProvider(dialect, db, fsys)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
