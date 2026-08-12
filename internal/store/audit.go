package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AuditEntry is an immutable record of an action an Operator took.
//
// Immutable means it: this package exports no way to change or remove one, and
// adding one would be a change to what dokkup is, not a convenience. A trail
// the accused can edit is not a trail. The only thing that ever touches a
// written entry is the operator being removed, which sets OperatorID to zero
// and leaves everything else standing.
type AuditEntry struct {
	ID int64

	// OperatorID names the operator who acted, or is zero when that operator
	// has since been removed. The entry survives them.
	OperatorID int64

	// OperatorEmail is the address the operator had at the moment they acted,
	// copied in then rather than joined now. It is what makes an entry still
	// legible after the operator is gone, and what stops a rename rewriting
	// history.
	OperatorEmail string

	// Action is what was done, in dokkup's own vocabulary.
	Action string

	// Target is what it was done to -- an App name, an operator's email
	// address. Empty for an action with no target, such as signing in.
	Target string

	// ConfigKey is WHICH configuration key the action changed, and there is
	// deliberately no field beside it for the value.
	//
	// Configuration is where an App's secrets live. An audit trail is kept
	// forever and read by everyone who can read the database, so recording
	// values would turn it into a permanent second copy of every credential any
	// operator has ever set -- including the ones since rotated away. Do not
	// add one, here or in the schema.
	ConfigKey string

	RecordedAt time.Time
}

// Audited describes an action to write into the trail.
//
// It is a separate type from [AuditEntry] so that the fields the store decides
// -- the id, the email as it stood, the instant -- cannot be supplied by a
// caller. An entry whose author chooses its timestamp is not evidence.
type Audited struct {
	// OperatorID is who acted. Required: nothing happens on this host that
	// nobody did.
	OperatorID int64

	// Action is what they did.
	Action string

	// Target is what they did it to, if anything.
	Target string

	// ConfigKey is which configuration key changed, if any. Never its value --
	// see [AuditEntry.ConfigKey].
	ConfigKey string
}

// Record writes one entry into the trail.
//
// The operator's email address is read and copied in by the same statement that
// inserts, so there is no gap in which an operator could be removed between
// being looked up and being recorded. An operator who does not exist inserts
// nothing and answers [ErrNotFound]: the alternative is an entry attributed to
// nobody, which is worse than no entry, because it would be read as a fact.
func (s *Store) Record(ctx context.Context, entry Audited) error {
	if entry.Action == "" {
		return fmt.Errorf("recording an audit entry for the operator %d: it names no action", entry.OperatorID)
	}

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_entries (operator_id, operator_email, action, target, config_key, recorded_at)
		 SELECT id, email, ?, ?, ?, ? FROM operators WHERE id = ?`,
		entry.Action, entry.Target, entry.ConfigKey, millis(now()), entry.OperatorID)
	if err != nil {
		return fmt.Errorf("recording that the operator %d did %q: %w", entry.OperatorID, entry.Action, err)
	}
	written, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("recording that the operator %d did %q: %w", entry.OperatorID, entry.Action, err)
	}
	if written == 0 {
		return fmt.Errorf("recording that the operator %d did %q: %w", entry.OperatorID, entry.Action, ErrNotFound)
	}
	return nil
}

// AuditEntries reads the trail, newest first, at most limit entries.
//
// Newest first because that is the only order it is ever read in, and it is the
// order the index is built in. A limit of zero or less reads the whole trail,
// which is what an export wants and what a screen must not ask for.
func (s *Store) AuditEntries(ctx context.Context, limit int) ([]AuditEntry, error) {
	// A negative LIMIT is SQLite's own way of saying "no limit", so the bound
	// parameter carries the intent rather than the query being built two ways.
	if limit <= 0 {
		limit = -1
	}

	return s.auditEntries(ctx, selectAuditEntries, limit)
}

// OperatorAuditEntries reads what one operator did, newest first.
//
// It matches on the recorded email address rather than on the operator id, so
// that the trail of an operator who has been removed is still findable -- their
// id became NULL, and the whole reason the address is copied in is that it did
// not.
//
// The cost of matching that way, which anybody reading the trail as evidence
// has to know: an address that is removed and later invited again is one
// address, so this returns what both operators did, in one list, with nothing
// in an entry to say which of them it was. The removed operator's entries carry
// [AuditEntry.OperatorID] of zero and the current operator's carry their id,
// which distinguishes the two while that operator is still there and stops
// distinguishing them the moment they are removed as well. Reusing an address
// is rare enough, and reading the trail wrongly is expensive enough, that this
// is documented rather than hidden behind a second column.
func (s *Store) OperatorAuditEntries(ctx context.Context, email string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = -1
	}
	return s.auditEntries(ctx, selectOperatorAuditEntries, normaliseEmail(email), limit)
}

// auditColumns is the select list both reads share, in the order
// [Store.auditEntries] scans.
const auditColumns = `id, operator_id, operator_email, action, target, config_key, recorded_at`

// The two reads of the trail, built once from that list for the same reason the
// operator reads are.
var (
	selectAuditEntries = `SELECT ` + auditColumns + `
		 FROM audit_entries ORDER BY recorded_at DESC, id DESC LIMIT ?`

	selectOperatorAuditEntries = `SELECT ` + auditColumns + `
		 FROM audit_entries WHERE operator_email = ? ORDER BY recorded_at DESC, id DESC LIMIT ?`
)

func (s *Store) auditEntries(ctx context.Context, query string, args ...any) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading the audit trail: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []AuditEntry
	for rows.Next() {
		var (
			entry AuditEntry
			// NULL once the operator is removed, which is the whole reason the
			// address is a column of its own.
			operatorID sql.NullInt64
			recordedAt int64
		)
		if err := rows.Scan(&entry.ID, &operatorID, &entry.OperatorEmail, &entry.Action,
			&entry.Target, &entry.ConfigKey, &recordedAt); err != nil {
			return nil, fmt.Errorf("reading the audit trail: %w", err)
		}
		entry.OperatorID, entry.RecordedAt = operatorID.Int64, instant(recordedAt)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the audit trail: %w", err)
	}
	return entries, nil
}
