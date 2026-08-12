package store

import (
	"context"
	"fmt"
	"time"
)

// Session is one operator's server-side proof that they signed in.
//
// It is server-side so that signing out revokes. See the schema comment on the
// table for why that is not negotiable.
type Session struct {
	// TokenHash is the hash of the token the browser holds, never the token.
	// The caller hashes; this package stores what it is given and matches it
	// verbatim.
	TokenHash string

	OperatorID int64

	CreatedAt time.Time

	// ExpiresAt is the instant after which the session is not a session. It is
	// compared in SQL rather than in Go -- see [Store.SessionOperator].
	ExpiresAt time.Time
}

// StartSession records a signed-in operator.
//
// It answers [ErrNotFound] when no such operator exists. That answer comes from
// the foreign key refusing the insert rather than from a read first: an
// operator removed between the two would otherwise leave a session pointing at
// nobody, which is exactly what the foreign key is on for.
//
// An expiry that has already passed is refused. A session that is dead on
// arrival is a caller bug, and storing it would mean the caller believes
// somebody is signed in who is not.
func (s *Store) StartSession(ctx context.Context, operatorID int64, tokenHash string, expiresAt time.Time) (Session, error) {
	if tokenHash == "" {
		return Session{}, fmt.Errorf("starting a session for the operator %d: no token hash to store", operatorID)
	}

	created := now()
	if !expiresAt.After(created) {
		return Session{}, fmt.Errorf("starting a session for the operator %d: it would expire at %s, which has passed",
			operatorID, expiresAt.UTC().Format(time.RFC3339))
	}

	session := Session{
		TokenHash:  tokenHash,
		OperatorID: operatorID,
		CreatedAt:  created,
		// Rounded the way it is stored, for the reason now() is.
		ExpiresAt: instant(millis(expiresAt)),
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, operator_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		session.TokenHash, session.OperatorID, millis(created), millis(expiresAt))
	switch {
	case isForeignKeyViolation(err):
		return Session{}, fmt.Errorf("starting a session for the operator %d: %w", operatorID, ErrNotFound)
	case isUniqueViolation(err):
		return Session{}, fmt.Errorf("starting a session for the operator %d: %w", operatorID, ErrExists)
	case err != nil:
		return Session{}, fmt.Errorf("starting a session for the operator %d: %w", operatorID, err)
	}
	return session, nil
}

// SessionOperator answers who is holding this session token, or [ErrNotFound].
//
// One call, because every request needs both halves and asking in two would
// mean a caller could act on a session whose operator was removed in between.
// The join is what makes the cascade in the schema visible: an operator who is
// gone has no sessions to find.
//
// The expiry is a WHERE clause and not a comparison the caller makes on the
// returned row. A caller who forgets that check has written an authentication
// bypass, and the only way to make forgetting impossible is for the expired row
// never to be returned at all. Nothing is deleted here -- an expired session is
// simply not a session -- because a read path that writes turns every page load
// into a writer, and [Store.DropExpiredSessions] is what actually clears them.
func (s *Store) SessionOperator(ctx context.Context, tokenHash string) (Operator, error) {
	operator, err := s.scanOperator(s.db.QueryRowContext(ctx,
		selectSessionOperator, tokenHash, millis(now())))
	if err != nil {
		// The token is not in the message on purpose. It is a credential, and
		// an error string reaches a log file.
		return Operator{}, fmt.Errorf("reading the operator holding a session: %w", err)
	}
	return operator, nil
}

// Session reads one session by its token hash, or answers [ErrNotFound],
// including when it has expired.
//
// It exists for the interface that shows an operator where they are signed in;
// authenticating a request is [Store.SessionOperator] and nothing else.
func (s *Store) Session(ctx context.Context, tokenHash string) (Session, error) {
	var (
		session              Session
		createdAt, expiresAt int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT token_hash, operator_id, created_at, expires_at
		 FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, millis(now()),
	).Scan(&session.TokenHash, &session.OperatorID, &createdAt, &expiresAt)
	if err != nil {
		return Session{}, fmt.Errorf("reading a session: %w", notFound(err))
	}
	session.CreatedAt, session.ExpiresAt = instant(createdAt), instant(expiresAt)
	return session, nil
}

// RevokeSession ends one session. This is what signing out does, and it is a
// DELETE: a row marked revoked is a row some future query forgets to filter,
// while a row that is not there cannot authenticate anybody.
//
// It answers [ErrNotFound] when there was nothing to end, so that a caller can
// tell a sign-out from a double-submitted one, and never treats a missing
// session as an error worth showing an operator.
func (s *Store) RevokeSession(ctx context.Context, tokenHash string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return fmt.Errorf("ending a session: %w", err)
	}
	ended, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("ending a session: %w", err)
	}
	if ended == 0 {
		return fmt.Errorf("ending a session: %w", ErrNotFound)
	}
	return nil
}

// RevokeOperatorSessions ends every session an operator holds and reports how
// many there were.
//
// It is how the Owner takes an account back without removing it, and how an
// operator signs out everywhere. Ending nothing is not an error: an operator
// with no sessions is already in the state this asks for.
func (s *Store) RevokeOperatorSessions(ctx context.Context, operatorID int64) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE operator_id = ?`, operatorID)
	if err != nil {
		return 0, fmt.Errorf("ending the sessions of the operator %d: %w", operatorID, err)
	}
	ended, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ending the sessions of the operator %d: %w", operatorID, err)
	}
	return ended, nil
}

// DropExpiredSessions removes the sessions that have run out and reports how
// many.
//
// They authenticate nobody either way, so this is housekeeping rather than a
// security measure: it stops a long-lived installation carrying a row for every
// sign-in it has ever seen. Keeping it out of the read path is deliberate --
// see [Store.SessionOperator].
func (s *Store) DropExpiredSessions(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, millis(now()))
	if err != nil {
		return 0, fmt.Errorf("dropping the expired sessions: %w", err)
	}
	dropped, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("dropping the expired sessions: %w", err)
	}
	return dropped, nil
}
