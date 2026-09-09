package sqldb_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jroedel/sion-backup/foundation/sqldb"
)

// TestTheFileIsOwnerOnly is the reason precreate exists. Left to the driver
// the mode follows the umask, and on a shared workstation that is a
// world-readable list of everything on the machine worth backing up.
func TestTheFileIsOwnerOnly(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX modes are not how Windows answers this")
	}

	path := filepath.Join(t.TempDir(), "nested", "sion-backup.db")

	db, err := sqldb.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("database has mode %o, want 600", perm)
	}
}

// TestForeignKeysAreEnforced checks the pragma actually took. SQLite parses a
// REFERENCES clause whether or not it intends to honour it, so a schema can
// look safe and enforce nothing.
func TestForeignKeysAreEnforced(t *testing.T) {
	ctx := context.Background()

	db, err := sqldb.Open(ctx, filepath.Join(t.TempDir(), "fk.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE parent (id INTEGER PRIMARY KEY);
		CREATE TABLE child (parent_id INTEGER REFERENCES parent(id));
	`); err != nil {
		t.Fatalf("creating the schema: %v", err)
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO child (parent_id) VALUES (404)`); err == nil {
		t.Error("inserted a child with no parent; foreign_keys is off")
	}
}

// TestReopeningKeepsTheData is the ordinary case, and it also proves Close
// releases the pool rather than only handing a connection back to it.
func TestReopeningKeepsTheData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopen.db")

	db, err := sqldb.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := db.ExecContext(ctx, `CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('kept')`); err != nil {
		t.Fatal(err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := sqldb.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer again.Close()

	var v string
	if err := again.QueryRowContext(ctx, `SELECT v FROM t`).Scan(&v); err != nil {
		t.Fatalf("reading it back: %v", err)
	}

	if v != "kept" {
		t.Errorf("got %q, want %q", v, "kept")
	}
}
