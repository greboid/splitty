package database_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/greboid/splitpayments/internal/database"
	"github.com/greboid/splitpayments/internal/testdb"
)

func TestMigrate(t *testing.T) {
	db, err := database.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := database.Migrate(db, "sqlite"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// A second run must be a no-op, not a re-apply.
	if err := database.Migrate(db, "sqlite"); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	var migrations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id > 0`).Scan(&migrations); err != nil {
		t.Fatalf("check goose_db_version: %v", err)
	}
	if migrations != 2 {
		t.Errorf("got %d recorded migrations, want 2", migrations)
	}
}

// TestMigratePostgres runs the same migration checks against a real server;
// set POSTGRES_TEST_DSN to enable it.
func TestMigratePostgres(t *testing.T) {
	if os.Getenv("POSTGRES_TEST_DSN") == "" {
		t.Skip("set POSTGRES_TEST_DSN to run migrations against Postgres")
	}
	db := testdb.Open(t)
	// testdb.Open migrated once; a second run must be a no-op, not a re-apply.
	if err := database.Migrate(db, "postgres"); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	var migrations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id > 0`).Scan(&migrations); err != nil {
		t.Fatalf("check goose_db_version: %v", err)
	}
	if migrations != 2 {
		t.Errorf("got %d recorded migrations, want 2", migrations)
	}
}
