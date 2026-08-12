package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// migration is one forward step in the schema.
//
// There is no down step, deliberately. A migration that has run has been run
// against operator data, and the only honest way back from a bad one is a
// restore of the file -- which is a copy, because the database is a single
// file. A down step that has never been run against real data is a promise
// nobody has tested.
type migration struct {
	// version is what schema_migrations records. It only ever grows, and a
	// number that has shipped is never reused or edited: an installation that
	// already applied it will not apply it again, so editing one means two
	// hosts running the same dokkup with different schemas.
	version int

	// name says what the step is for, in the file rather than in the database,
	// because it is documentation and not state.
	name string

	// statements are executed in order inside one transaction.
	statements []string
}

// migrations is the whole history of the schema, in order.
//
// Append only.
var migrations = []migration{
	{
		version: 1,
		name:    "operators, sessions and audit entries",
		statements: []string{
			// Operators are the people who sign in. Every one of them can act
			// on the Dokku Host with root-equivalent power, so this table is
			// the entire access control model there is.
			//
			// email is stored already normalised (see normaliseEmail) and is
			// unique through an index rather than a column constraint, so that
			// the uniqueness is over the normalised form and a future change
			// to the index does not have to rewrite the table.
			//
			// password_hash is opaque here. The store keeps what it is given
			// and never hashes or compares -- choosing the algorithm is not a
			// storage decision, and a schema that knew the algorithm would have
			// to migrate to change it.
			`CREATE TABLE operators (
				id            INTEGER PRIMARY KEY,
				email         TEXT    NOT NULL,
				name          TEXT    NOT NULL,
				password_hash TEXT    NOT NULL,
				is_owner      INTEGER NOT NULL DEFAULT 0 CHECK (is_owner IN (0, 1)),
				created_at    INTEGER NOT NULL,
				updated_at    INTEGER NOT NULL
			) STRICT`,

			`CREATE UNIQUE INDEX operators_email ON operators (email)`,

			// There is exactly one Owner, and the schema is what says so.
			//
			// A partial unique index over is_owner covers only the rows where
			// it is true, so every non-owner row is outside the index and any
			// number of them coexist, while a second owner collides with the
			// first. Enforcing this in Go instead would mean a check and an
			// insert with a gap between them, and the one thing that must not
			// be racy on this host is who controls it. Measured: with three
			// rows inserted, the second is_owner = 1 was refused with
			// "UNIQUE constraint failed: operators.is_owner (2067)".
			`CREATE UNIQUE INDEX operators_single_owner ON operators (is_owner) WHERE is_owner`,

			// Sessions are server-side so that signing out revokes rather than
			// merely forgets. A self-contained token verified by signature
			// alone stays valid until it expires no matter what the operator
			// clicks, and an operator who removes another operator has to be
			// able to end their access now.
			//
			// token_hash, never the token. A database that has been read must
			// not be a database that can be signed in with, which is the same
			// reason password_hash is a hash.
			//
			// ON DELETE CASCADE, so that removing an operator takes their
			// access with it in the same statement. That is only true with
			// foreign_keys on, which is why Open sets it per-connection.
			`CREATE TABLE sessions (
				token_hash  TEXT    PRIMARY KEY,
				operator_id INTEGER NOT NULL REFERENCES operators (id) ON DELETE CASCADE,
				created_at  INTEGER NOT NULL,
				expires_at  INTEGER NOT NULL
			) STRICT`,

			`CREATE INDEX sessions_operator ON sessions (operator_id)`,
			`CREATE INDEX sessions_expires_at ON sessions (expires_at)`,

			// Audit entries are immutable: written once, read, and never
			// updated or deleted. There is no exported call in this package
			// that changes one, and there must not be -- a trail that can be
			// edited by the same operator it accuses is not evidence.
			//
			// config_key records WHICH configuration key an action changed and
			// the schema has nowhere to put its value. That is the point: app
			// configuration is where secrets live, and an audit trail holding
			// them would be a second, permanent copy of every credential an
			// operator ever set. Do not add a config_value column.
			//
			// operator_email is copied in at the time rather than joined at
			// read time, and operator_id is ON DELETE SET NULL. An operator who
			// is removed must not take their trail with them, and must not
			// leave rows naming an id that means nothing.
			`CREATE TABLE audit_entries (
				id             INTEGER PRIMARY KEY,
				operator_id    INTEGER          REFERENCES operators (id) ON DELETE SET NULL,
				operator_email TEXT    NOT NULL,
				action         TEXT    NOT NULL,
				target         TEXT    NOT NULL DEFAULT '',
				config_key     TEXT    NOT NULL DEFAULT '',
				recorded_at    INTEGER NOT NULL
			) STRICT`,

			// Newest first is the only order the trail is ever read in, and id
			// breaks the tie between two entries recorded in the same
			// millisecond.
			`CREATE INDEX audit_entries_recorded_at ON audit_entries (recorded_at DESC, id DESC)`,
		},
	},
	{
		version: 2,
		name:    "setup tokens",
		statements: []string{
			// A Setup Token is what ADR-0007 puts between the installer and
			// the Owner. It lives in a table of its own rather than as a
			// column on an operator row, because the row it creates does not
			// exist yet while the token does, and because a token is spent by
			// deleting it -- a "used_at" column would leave a spent credential
			// in the file forever and make every read of it a filter somebody
			// can forget.
			//
			// token_hash, never the token, for the same reason sessions store
			// a hash: a database that has been read must not be a database
			// that can claim ownership of the host.
			//
			// It is the primary key, which is also the whole of the uniqueness
			// this table needs. There is at most one live token by policy --
			// [Store.IssueSetupToken] replaces the outstanding one -- and that
			// policy belongs in the statement that writes, not in a constraint
			// that would have to be dropped the day a second one is wanted.
			//
			// No foreign key anywhere: the token is deliberately not tied to
			// an operator, an email address or an installation id. Whoever
			// holds it becomes the Owner, and the only thing that limits that
			// is the clock and the single spending.
			`CREATE TABLE setup_tokens (
				token_hash TEXT    PRIMARY KEY,
				created_at INTEGER NOT NULL,
				expires_at INTEGER NOT NULL
			) STRICT`,
		},
	},
}

// migrate applies every migration this binary knows and the database has not.
//
// Each one runs inside its own transaction, so a statement that fails leaves
// the schema as it was rather than half-way through a step -- SQLite rolls back
// DDL like anything else. The version is recorded inside that same transaction,
// which is what makes a restart idempotent: a step is applied and recorded
// together or neither.
//
// Startup, not a command, and forward-only. See [migration].
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("creating the schema version table: %w", err)
	}

	applied, err := s.appliedVersion(ctx)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if m.version <= applied {
			continue
		}
		if err := s.apply(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// appliedVersion reports the highest migration this database has recorded, or
// zero for a database that has never been migrated.
//
// MAX over an empty table is one row holding NULL rather than no rows, so the
// scan target is nullable; reading it as an int would fail on exactly the
// empty-directory case this has to handle.
func (s *Store) appliedVersion(ctx context.Context) (int, error) {
	var version sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("reading the schema version: %w", err)
	}
	return int(version.Int64), nil
}

func (s *Store) apply(ctx context.Context, m migration) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting migration %d (%s): %w", m.version, m.name, err)
	}
	defer func() {
		// Rollback after a commit answers sql.ErrTxDone and means nothing.
		if rollback := tx.Rollback(); rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rolling migration %d (%s) back: %w", m.version, m.name, rollback))
		}
	}()

	for _, statement := range m.statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("applying migration %d (%s): %w", m.version, m.name, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.version, millis(now()),
	); err != nil {
		return fmt.Errorf("recording migration %d (%s): %w", m.version, m.name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing migration %d (%s): %w", m.version, m.name, err)
	}
	return nil
}
