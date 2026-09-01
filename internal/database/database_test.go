package database

import (
	"path/filepath"
	"testing"
)

func TestMigrate(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// A second run must be a no-op, not a re-apply.
	if err := Migrate(db); err != nil {
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
