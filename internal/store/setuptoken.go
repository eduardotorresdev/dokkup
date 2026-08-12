package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SetupToken is the single-use, short-lived credential that ADR-0007 puts
// between an installation and its Owner.
//
// There is no field for the token itself, here or in the schema. The token
// exists once, in the terminal the installer printed it into, and dokkup keeps
// only what is needed to recognise it coming back.
type SetupToken struct {
	// TokenHash is what the caller hashed the token to. This package stores
	// what it is given and matches it verbatim -- hashing is
	// [github.com/eduardotorresdev/dokkup/internal/secret]'s decision, not the
	// schema's.
	TokenHash string

	CreatedAt time.Time

	// ExpiresAt is the instant after which the token is not a token. It is
	// compared in SQL, in the same statement that spends it -- see
	// [Store.RedeemSetupToken].
	ExpiresAt time.Time
}

// IssueSetupToken stores the hash of a freshly minted token, replacing any
// outstanding one, so there is at most one live token at a time.
//
// Replacing rather than accumulating is what makes reissuing a revocation: an
// operator who reruns `dokkup setup-token` because the first one scrolled out
// of their terminal, or because they suspect it was seen, has to end up with
// exactly one credential that works. Tokens that merely pile up would mean the
// oldest printed one still owns the host.
//
// It answers [ErrOwnerExists] when this installation already has an Owner, and
// that refusal is the WHERE of the insert rather than a read this code makes
// first. ADR-0007 turns on it: a token issued after ownership is settled would
// be an unauthenticated path to taking the host over, so the decision has to
// be the database's and not a check with a gap after it.
func (s *Store) IssueSetupToken(ctx context.Context, tokenHash string, expiresAt time.Time) (SetupToken, error) {
	if tokenHash == "" {
		return SetupToken{}, errors.New("issuing a setup token: no token hash to store")
	}

	created := now()
	if !expiresAt.After(created) {
		return SetupToken{}, fmt.Errorf("issuing a setup token: it would expire at %s, which has passed",
			expiresAt.UTC().Format(time.RFC3339))
	}

	token := SetupToken{
		TokenHash: tokenHash,
		CreatedAt: created,
		// Rounded the way it is stored, for the reason now() is.
		ExpiresAt: instant(millis(expiresAt)),
	}

	// One transaction, so that the window in which the old token is gone and
	// the new one is not yet there does not exist for another process. SQLite
	// takes its write lock at the DELETE and holds it through the INSERT, so
	// the NOT EXISTS below is read with no writer able to commit an Owner
	// alongside it.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SetupToken{}, fmt.Errorf("issuing a setup token: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM setup_tokens`); err != nil {
		return SetupToken{}, fmt.Errorf("revoking the outstanding setup token: %w", err)
	}

	result, err := tx.ExecContext(ctx,
		`INSERT INTO setup_tokens (token_hash, created_at, expires_at)
		 SELECT ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM operators WHERE is_owner)`,
		token.TokenHash, millis(created), millis(expiresAt))
	if err != nil {
		return SetupToken{}, fmt.Errorf("issuing a setup token: %w", err)
	}
	issued, err := result.RowsAffected()
	if err != nil {
		return SetupToken{}, fmt.Errorf("issuing a setup token: %w", err)
	}
	// Nothing was inserted only because the subquery found an Owner, and the
	// rollback that follows takes the DELETE with it: a refused issue leaves
	// the outstanding token exactly as it was.
	if issued == 0 {
		return SetupToken{}, fmt.Errorf("issuing a setup token: %w", ErrOwnerExists)
	}

	if err := tx.Commit(); err != nil {
		return SetupToken{}, fmt.Errorf("issuing a setup token: %w", err)
	}
	return token, nil
}

// RedeemSetupToken spends the token and creates the Owner in one transaction.
//
// One transaction is the whole design. The token is revoked by the same
// transaction that creates the Owner -- not before it, which would burn the
// operator's only credential on a request that then failed validation, and not
// after it, which would leave a moment in which a second redemption arriving
// on another connection reads a token that is about to be spent. Two
// simultaneous redemptions of the same token therefore create one Owner: the
// loser's DELETE matches no row.
//
// It answers [ErrNotFound] when there is no live token with that hash and
// [ErrOwnerExists] when there is already an Owner. Unknown, expired and
// already spent are deliberately the same answer, because they are the same
// answer to the caller and telling them apart would let somebody with a used
// token learn that it was once real.
//
// The insert is spelled out here rather than going through
// [Store.CreateOwner], because it has to run on the transaction that spends the
// token. Threading an executor through the operator writes would put an
// argument on every one of them for the sake of this single path, and the
// alternative -- creating the Owner outside the transaction -- is the race this
// function exists to close.
func (s *Store) RedeemSetupToken(ctx context.Context, tokenHash, email, name, passwordHash string) (Operator, error) {
	email = normaliseEmail(email)
	switch {
	case tokenHash == "":
		return Operator{}, errors.New("redeeming a setup token: no token hash to look up")
	case email == "":
		return Operator{}, errors.New("redeeming a setup token: no email address to sign in with")
	case passwordHash == "":
		return Operator{}, fmt.Errorf("redeeming a setup token: no password hash to store for %s", email)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Operator{}, fmt.Errorf("redeeming a setup token: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Expiry is in the WHERE and not a comparison on a row read first, for the
	// reason [Store.SessionOperator] gives: a caller who forgets the check has
	// written the bypass, and a row that is never returned cannot be forgotten
	// about. The delete is the check -- exactly one row means the token was
	// live and is now this transaction's to spend.
	spent, err := tx.ExecContext(ctx,
		`DELETE FROM setup_tokens WHERE token_hash = ? AND expires_at > ?`, tokenHash, millis(now()))
	if err != nil {
		// The hash stays out of the message. It is not the token, but it is
		// the only thing standing between a leaked log line and an attacker
		// who can also read the database.
		return Operator{}, fmt.Errorf("spending a setup token: %w", err)
	}
	tokens, err := spent.RowsAffected()
	if err != nil {
		return Operator{}, fmt.Errorf("spending a setup token: %w", err)
	}
	// Zero, because one is the only other answer: token_hash is the primary
	// key, so no delete of it can match twice.
	if tokens == 0 {
		return Operator{}, fmt.Errorf("spending a setup token: %w", ErrNotFound)
	}

	stamp := now()
	owner := Operator{
		Email:        email,
		Name:         name,
		PasswordHash: passwordHash,
		IsOwner:      true,
		CreatedAt:    stamp,
		UpdatedAt:    stamp,
	}

	created, err := tx.ExecContext(ctx,
		`INSERT INTO operators (email, name, password_hash, is_owner, created_at, updated_at)
		 VALUES (?, ?, ?, 1, ?, ?)`,
		owner.Email, owner.Name, owner.PasswordHash, millis(stamp), millis(stamp))
	if err != nil {
		// Told apart by the column SQLite names, as [Store.CreateOwner] does
		// and for the reason given there. A live token and an existing Owner
		// is the state left by an installation whose token was issued before
		// somebody else redeemed another one.
		if isUniqueViolation(err) {
			if strings.Contains(err.Error(), "is_owner") {
				return Operator{}, fmt.Errorf("creating the owner %s: %w", email, ErrOwnerExists)
			}
			return Operator{}, fmt.Errorf("creating the owner %s: %w", email, ErrExists)
		}
		return Operator{}, fmt.Errorf("creating the owner %s: %w", email, err)
	}

	id, err := created.LastInsertId()
	if err != nil {
		return Operator{}, fmt.Errorf("reading back the id of the owner %s: %w", email, err)
	}
	owner.ID = id

	if err := tx.Commit(); err != nil {
		return Operator{}, fmt.Errorf("redeeming a setup token for %s: %w", email, err)
	}
	return owner, nil
}

// HasOwner reports whether this installation has been set up.
//
// A boolean and not [Store.Owner] with its error read for [ErrNotFound],
// because the question "is setup done" is asked by an unauthenticated request
// and must not hand back the Owner's email address and password hash to answer
// it. EXISTS also stops at the first row, where the read of an operator would
// carry every column across.
func (s *Store) HasOwner(ctx context.Context) (bool, error) {
	var owned bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM operators WHERE is_owner)`).Scan(&owned); err != nil {
		return false, fmt.Errorf("reading whether this installation has an owner: %w", err)
	}
	return owned, nil
}

// DropExpiredSetupTokens removes tokens that have run out, reporting how many.
//
// Housekeeping and not a security measure: a token past its expiry redeems
// nothing either way, because the expiry is in the WHERE of the statement that
// spends it. What this buys is that an installation which was set up months ago
// does not keep a row naming a credential that was once printed on somebody's
// screen.
func (s *Store) DropExpiredSetupTokens(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM setup_tokens WHERE expires_at <= ?`, millis(now()))
	if err != nil {
		return 0, fmt.Errorf("dropping the expired setup tokens: %w", err)
	}
	dropped, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("dropping the expired setup tokens: %w", err)
	}
	return dropped, nil
}
