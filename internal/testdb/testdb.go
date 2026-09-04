// Package testdb provides migrated databases for tests: a private
// in-memory SQLite by default, or Postgres when POSTGRES_TEST_DSN is set.
//
// Each Open call gets its own database — on SQLite an in-memory file, on
// Postgres one named after the process id and a call counter (created, and
// dropped again when the test ends). Fresh-per-call matches how tests use
// apps: a second newApp models a separate instance (its own setup page and
// data), and go test runs package binaries in parallel.
package testdb

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/greboid/splitpayments/internal/database"
)

var openCount int64

// Open returns a migrated database private to the test.
func Open(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		return openSQLite(t)
	}
	db, err := openPostgres(dsn)
	if err != nil {
		t.Fatalf("postgres test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	// One connection keeps a private in-memory DB alive for this test only.
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := database.Migrate(db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func openPostgres(dsn string) (*sql.DB, error) {
	testDSN, dbName, err := perCallDatabase(dsn)
	if err != nil {
		return nil, err
	}
	admin, err := database.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", dsn, err)
	}
	defer admin.Close()
	// The pid/counter suffix makes name collisions impossible between live
	// processes; FORCE clears leftovers from a crashed one.
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + quoteIdent(dbName) + ` WITH (FORCE)`); err != nil {
		// Pre-PG13 servers lack WITH (FORCE); nothing can be connected to a
		// database of this freshly-derived name, so the plain form is safe.
		if _, err2 := admin.Exec(`DROP DATABASE IF EXISTS ` + quoteIdent(dbName)); err2 != nil {
			return nil, fmt.Errorf("drop test database: %w (%w)", err, err2)
		}
	}
	if _, err := admin.Exec(`CREATE DATABASE ` + quoteIdent(dbName)); err != nil {
		return nil, fmt.Errorf("create test database: %w", err)
	}
	db, err := database.Open("postgres", testDSN)
	if err != nil {
		return nil, err
	}
	if err := database.Migrate(db, "postgres"); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// perCallDatabase derives the DSN of a per-call database from dsn, returning
// it alongside the bare database name.
func perCallDatabase(dsn string) (string, string, error) {
	n := atomic.AddInt64(&openCount, 1)
	suffix := fmt.Sprintf("_t%d_%d", os.Getpid(), n)
	if u, err := url.Parse(dsn); err == nil &&
		(u.Scheme == "postgres" || u.Scheme == "postgresql") {
		name := strings.TrimPrefix(u.Path, "/") + suffix
		u.Path = "/" + name
		return u.String(), name, nil
	}
	// Key/value form: host=… dbname=…
	re := regexp.MustCompile(`dbname=(\S+)`)
	m := re.FindStringSubmatch(dsn)
	if m == nil {
		return "", "", fmt.Errorf("cannot find a database name in %q", dsn)
	}
	name := m[1] + suffix
	return re.ReplaceAllString(dsn, "dbname="+name), name, nil
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
