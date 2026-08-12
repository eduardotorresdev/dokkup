// Package store keeps the small part of the world dokkup is the authority for.
//
// It exists because authentication cannot be read from Dokku. Dokku owns
// everything about applications and dokkup reads that live, so the only reason
// for a database here at all is dokkup's Own State: the operators who sign in,
// their sessions, and the audit trail of what they did. Nothing about an App is
// persisted, ever -- see docs/adr/0002-dokku-is-the-source-of-truth.md, and
// re-read it before adding a column that looks like a cache.
//
// One SQLite file under the data directory, which is what makes backup a copy
// and removal a delete, and is what lets the uninstall contract in ADR-0008 be
// honoured at all.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	// The driver is modernc.org/sqlite, which is SQLite translated to Go rather
	// than bound to it. Importing it registers "sqlite" with database/sql and
	// also gives us *sqlite.Error, which is the only way to tell a duplicate
	// email from a broken disk. mattn/go-sqlite3 is the faster and
	// better-known choice and it is not available to us: it needs cgo, while
	// .goreleaser.yaml and the Makefile both build with CGO_ENABLED=0 so that
	// one binary runs on any glibc or musl host without a toolchain. A driver
	// that cannot be in a release is not a driver.
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrNotFound reports that the row asked for is not there. It is also what an
// expired session answers, because an expired session is not a session.
var ErrNotFound = errors.New("not found")

// ErrExists reports that an identity is already taken -- an operator with that
// email address. Nothing was written when it is returned.
var ErrExists = errors.New("already exists")

// ErrOwnerExists reports that this installation already has its Owner. It is
// separate from [ErrExists] because ADR-0007 turns on the difference: reissuing
// a Setup Token is allowed only while no Owner exists, and "somebody already
// owns this host" is a different answer from "that email is taken".
var ErrOwnerExists = errors.New("this installation already has an owner")

// ErrOwnerProtected reports a refusal to remove the Owner. The Owner is the
// only operator who may manage operators, so removing them leaves an
// installation nobody can administer and no supported way back.
var ErrOwnerProtected = errors.New("the owner cannot be removed")

// Permissions on the database and the directory holding it.
//
// The database holds password hashes and session tokens, so a host account that
// can read it can sign in as any operator. dokkup runs as its own unprivileged
// user (ADR-0005) and no other account on the host has business here.
const (
	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600
)

// busyTimeout is how long a connection waits for a writer to finish before
// giving up with SQLITE_BUSY. SQLite serialises writers even under
// write-ahead logging; without a timeout the loser of a race gets an error
// where waiting a few milliseconds would have done.
const busyTimeout = 5 * time.Second

// Store is dokkup's Own State. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens the database at path, creating it if it is not there, and brings
// the schema up to date before returning.
//
// Migrations run here rather than from a command, because there is no moment
// between the binary being replaced and it serving requests in which anybody
// would run one. An installation that starts is an installation that is
// migrated, or it is an installation that refused to start.
//
// The connection settings are not defaults and every one of them is load-bearing:
//
//   - foreign_keys is off by default in SQLite and is per-connection, not a
//     property of the file, so setting it once after opening would leave every
//     other connection in the pool without it. It is passed in the DSN, which
//     the driver applies to each connection as it is made -- measured, four
//     simultaneous connections from one pool each reported
//     "PRAGMA foreign_keys" = 1, and an insert referencing a row that does not
//     exist failed with "FOREIGN KEY constraint failed (787)".
//
//   - journal_mode = WAL is a property of the file and survives in its header,
//     so it is set once here; measured, every later connection reported
//     "PRAGMA journal_mode" = wal without being told. It is what lets a reader
//     run while a writer commits, which matters because a page load may audit
//     while another operator reads the trail.
//
//   - busy_timeout, for the reason given where it is declared.
//
// Note that unknown DSN parameters are ignored in silence by this driver --
// measured, a DSN carrying "_bogus_param=1" opened without complaint -- so a
// setting here is only real if something asserts it. store_test.go does.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := prepareFiles(path); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("opening the database at %s: %w", path, err)
	}

	store := &Store{db: db}

	if err := store.enableWAL(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := restrictSidecars(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the database. Closing the last connection is what makes
// SQLite checkpoint the write-ahead log into the database file and remove the
// "-wal" and "-shm" files beside it, so a store that is closed leaves one file
// on disk, which is the whole point of a backup being a copy.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("closing the database: %w", err)
	}
	return nil
}

// prepareFiles makes the directory and the database file exist with
// permissions no other account on the host can read through.
//
// The file is created before SQLite sees it, because SQLite would create it
// with 0644 masked by the umask and there is no window in which a file holding
// password hashes should be world-readable. A zero-length file is a valid empty
// database, so handing SQLite one costs nothing.
//
// Both are also chmod-ed rather than only created, so that a database from an
// older dokkup, or one a restore put back with the archive's own mode, is
// tightened on the next start instead of staying open forever.
func prepareFiles(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("creating the data directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("restricting the data directory %s: %w", dir, err)
	}

	// The path is dokkup's own database, [hostpaths.DB], or the one a test
	// chose; there is no operator input anywhere near it.
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm) //nolint:gosec // G304: the path is the caller's own data directory
	if err != nil {
		return fmt.Errorf("creating the database file %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("creating the database file %s: %w", path, err)
	}
	if err := os.Chmod(path, filePerm); err != nil {
		return fmt.Errorf("restricting the database file %s: %w", path, err)
	}
	return nil
}

// restrictSidecars tightens the two files write-ahead logging puts beside the
// database.
//
// SQLite gives them the mode of the database file itself -- measured, a
// database pre-created 0600 got "-wal" and "-shm" at 0600, and the same probe
// against a database left at 0644 got both at 0644 -- so with prepareFiles
// having already run this changes nothing today. It is here because that
// inheritance is the VFS's behaviour rather than a promise, and the cost of
// being wrong is a readable copy of every session token.
//
// A missing sidecar is not an error: they exist only once something has been
// written through the log, and a store opened and closed without a write may
// never have had one.
func restrictSidecars(path string) error {
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := path + suffix
		if err := os.Chmod(sidecar, filePerm); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("restricting %s: %w", sidecar, err)
		}
	}
	return nil
}

// dsn builds the URI the driver is opened with.
//
// The path is escaped because SQLite reads a "file:" DSN as a URI: a percent
// sign in a directory name would otherwise be read as the start of an escape,
// and a question mark would start the query. Neither appears in
// [hostpaths.DB], but a test temporary directory is not ours to choose.
func dsn(path string) string {
	escaped := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(path))
	return fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(%d)",
		escaped, busyTimeout.Milliseconds())
}

// enableWAL switches the file to write-ahead logging and refuses to go on if it
// did not take.
//
// PRAGMA journal_mode answers with the mode it ended up in rather than with an
// error, so the only way to know it worked is to read the answer. A filesystem
// that cannot support the shared-memory file -- a network mount is the usual
// one -- leaves the database in "delete" mode, where a reader blocks a writer,
// and dokkup would look intermittently wedged rather than broken.
func (s *Store) enableWAL(ctx context.Context) error {
	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode); err != nil {
		return fmt.Errorf("switching the database to write-ahead logging: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("switching the database to write-ahead logging: it stayed in %q mode", mode)
	}
	return nil
}

// constraint reports whether err is SQLite refusing a write because of the
// named constraint kind.
//
// The codes are the extended result codes, matched rather than the message,
// because the message is prose the library is free to reword. Measured against
// this driver's own schema: a second owner through the partial unique index
// gives "UNIQUE constraint failed: operators.is_owner (2067)", a duplicate
// address gives "UNIQUE constraint failed: operators.email (2067)", and a
// reference to a row that does not exist gives
// "FOREIGN KEY constraint failed (787)", all carried on a *sqlite.Error.
func constraint(err error, codes ...int) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}
	return slices.Contains(codes, serr.Code())
}

// isUniqueViolation reports a row SQLite refused because another one already
// holds that value.
//
// Two codes, not one. A collision on a UNIQUE index and a collision on a
// PRIMARY KEY are the same event to a caller and are not the same code:
// measured, a duplicate operators.email gave 2067 while a duplicate
// sessions.token_hash -- the sessions table's primary key -- gave
// SQLITE_CONSTRAINT_PRIMARYKEY (1555), with the message still reading
// "UNIQUE constraint failed: sessions.token_hash". Matching only 2067 would
// hand a caller the driver's raw error for the one case where the token they
// generated has been seen before.
func isUniqueViolation(err error) bool {
	return constraint(err, sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
}

func isForeignKeyViolation(err error) bool {
	return constraint(err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
}

// millis is how every instant is stored: milliseconds since the Unix epoch, as
// an INTEGER.
//
// Not a formatted string, so that comparison in SQL is arithmetic and cannot be
// thrown by a timezone or by a format that sorts differently from the instants
// it names. Milliseconds rather than seconds because a session that expires is
// compared against a bound instant, and rounding a stored expiry down by up to
// a second is a session that outlives its own deadline.
func millis(t time.Time) int64 { return t.UnixMilli() }

// now is the clock every row is stamped from.
//
// UTC at the source, so that a host whose timezone is changed does not produce
// a trail that appears to jump, and already rounded to the precision the column
// keeps. Without that rounding a struct returned by a create would carry
// nanoseconds the database never stored, and the same row read back a moment
// later would compare as earlier than the value its own creation returned.
func now() time.Time { return instant(time.Now().UnixMilli()) }

// instant is millis read back. UTC, because a time read out of the database
// carries no zone of its own and pretending it is the host's would make the
// same row print differently on two machines.
func instant(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
