// Package sqldb opens the one SQLite database this program uses.
//
// Two domains persist into the same file — the backup plan and the run history
// — and a second *sql.DB on the same SQLite database would put two independent
// pools behind stores that each assume a single writer. Opening here once and
// handing the handle to each store keeps that assumption true however many
// domains are added.
//
// The driver is modernc.org/sqlite, a pure-Go translation of SQLite. No cgo
// means no C toolchain, which is what makes `GOOS=windows make release` from a
// Linux box produce a working binary rather than a linker error.
//
// # Why the handle is one connection and not a pool
//
// [Open] returns a [DB]: a single connection with the pool that produced it
// kept out of reach.
//
// Capping the pool at one connection and leaving it exported would not be
// enough, because the failure that produces is worse than an error. With one
// connection permitted and that connection checked out, any use of the pool
// blocks waiting for a connection that will never be returned — a deadlock
// with no message and no timeout, which is a genuinely difficult thing to
// diagnose from a hung status page. So the pool is an unexported field: the
// mistake cannot be written rather than being discouraged by a comment.
//
// The pragmas argue for it too. foreign_keys is per-connection, so a pool only
// ever applies it reliably because the pool is capped at one.
package sqldb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// DB is the handle a store is given: one connection, and the pool behind it
// held privately so nobody can deadlock on it.
//
// The embedded *sql.Conn supplies ExecContext, QueryContext, QueryRowContext
// and BeginTx with the signatures a store already uses.
type DB struct {
	*sql.Conn

	// pool is unexported on purpose. See the package comment: reaching it
	// while the connection is checked out blocks forever.
	pool *sql.DB

	path string
}

// Close releases the connection and then the pool.
//
// This shadows the embedded (*sql.Conn).Close, whose job is to hand a
// connection back to a pool the caller cannot see and would have no way to
// close afterwards. Closing a DB means closing the database.
func (d *DB) Close() error {
	if d == nil {
		return nil
	}

	var first error

	if d.Conn != nil {
		if err := d.Conn.Close(); err != nil {
			first = err
		}
	}

	if d.pool != nil {
		if err := d.pool.Close(); err != nil && first == nil {
			first = err
		}
	}

	return first
}

// Path reports the file the database was opened from.
func (d *DB) Path() string { return d.path }

// Open opens (creating if needed) the database at path.
//
// WAL keeps the status page's reads from blocking a run's writes, busy_timeout
// makes a contended write wait rather than fail with SQLITE_BUSY, and
// foreign_keys is on so a schema that declares a reference actually gets it
// enforced — SQLite ignores them by default.
//
// The file is created 0600 before SQLite sees it. Left to itself SQLite honours
// the process umask, which on most systems yields a world-readable list of
// every directory on this machine worth backing up.
func Open(ctx context.Context, path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("sqldb: creating directory for %s: %w", path, err)
		}
	}

	if err := precreate(path); err != nil {
		return nil, err
	}

	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"+
			"&_pragma=foreign_keys(1)&_pragma=synchronous(normal)", path)

	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqldb: opening %s: %w", path, err)
	}

	// One connection, and it never expires. The pool must not close and
	// reopen the connection underneath us: that would silently re-apply — or
	// fail to re-apply — the pragmas above.
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	pool.SetConnMaxLifetime(0)
	pool.SetConnMaxIdleTime(0)

	if err := pool.PingContext(ctx); err != nil {
		pool.Close()

		return nil, fmt.Errorf("sqldb: connecting to %s: %w", path, err)
	}

	// Checked out for the lifetime of the DB. After this the pool has no
	// connection left to give, which is why it is unexported.
	conn, err := pool.Conn(ctx)
	if err != nil {
		pool.Close()

		return nil, fmt.Errorf("sqldb: pinning a connection to %s: %w", path, err)
	}

	// WAL creates -wal and -shm siblings on first write; they hold the same
	// data as the main file and need the same mode.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0o600)
	}

	return &DB{Conn: conn, pool: pool, path: path}, nil
}

// precreate makes the file 0600 before the driver can make it something else.
//
// O_EXCL so that an existing database keeps whatever mode it already has: an
// operator who deliberately widened it is not overruled here, and a race with
// a second process starting at the same moment does not truncate anything.
func precreate(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	switch {
	case err == nil:
		return f.Close()
	case errors.Is(err, fs.ErrExist):
		return nil
	default:
		return fmt.Errorf("sqldb: creating %s: %w", path, err)
	}
}
