package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Operator is a person who signs in to dokkup.
//
// Operators exist only in dokkup and have no relationship to Dokku's own SSH
// users. Every one of them can act on the Dokku Host with root-equivalent
// power, so there is no permission field here beyond IsOwner: the only
// distinction dokkup draws is who may manage operators.
type Operator struct {
	ID int64

	// Email is the identity an operator signs in with, normalised. Two
	// operators cannot share one.
	Email string

	// Name is what the interface calls them. It is not an identity and is not
	// unique.
	Name string

	// PasswordHash is whatever the caller stored. This package never produces,
	// interprets or compares it -- see the schema comment on the column.
	PasswordHash string

	// IsOwner marks the single operator who may create, edit and remove other
	// operators. At most one row in the database has it, enforced by a partial
	// unique index rather than by this package.
	IsOwner bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// The select lists every operator read shares, in the order
// [Store.scanOperator] scans. Two literals rather than one built from a slice:
// the scan spells its targets out by hand either way, so a generated list makes
// two places to edit look like one, and there is no concatenation for a linter
// to read as an injection.
const (
	operatorColumns = `id, email, name, password_hash, is_owner, created_at, updated_at`

	// Qualified, for the one read that joins. See [Store.SessionOperator].
	joinedOperatorColumns = `operators.id, operators.email, operators.name, operators.password_hash, ` +
		`operators.is_owner, operators.created_at, operators.updated_at`
)

// The reads this package makes of the operators table.
const (
	selectOperatorByID    = `SELECT ` + operatorColumns + ` FROM operators WHERE id = ?`
	selectOperatorByEmail = `SELECT ` + operatorColumns + ` FROM operators WHERE email = ?`
	selectOwner           = `SELECT ` + operatorColumns + ` FROM operators WHERE is_owner`
	selectOperators       = `SELECT ` + operatorColumns + ` FROM operators ORDER BY is_owner DESC, id`

	selectSessionOperator = `SELECT ` + joinedOperatorColumns + `
		 FROM sessions JOIN operators ON operators.id = sessions.operator_id
		 WHERE sessions.token_hash = ? AND sessions.expires_at > ?`
)

// notFound turns SQLite's "no rows" into this package's [ErrNotFound] and
// leaves every other error alone.
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// CreateOwner creates the single operator who may manage other operators.
//
// It is the act ADR-0007 puts behind the Setup Token, and it is the only way an
// owner row is ever written -- there is no call that promotes an operator
// afterwards, because "who owns this host" changing quietly is the failure the
// token exists to prevent.
//
// It answers [ErrOwnerExists] when this installation already has an Owner. That
// answer comes from the insert being refused by the partial unique index, not
// from a check this code ran first: two Setup Token redemptions arriving
// together must not both succeed, and only the database can decide that.
func (s *Store) CreateOwner(ctx context.Context, email, name, passwordHash string) (Operator, error) {
	return s.createOperator(ctx, email, name, passwordHash, true)
}

// CreateOperator creates an operator the Owner invited.
//
// It answers [ErrExists] when that email address is already an operator's.
func (s *Store) CreateOperator(ctx context.Context, email, name, passwordHash string) (Operator, error) {
	return s.createOperator(ctx, email, name, passwordHash, false)
}

func (s *Store) createOperator(ctx context.Context, email, name, passwordHash string, owner bool) (Operator, error) {
	email = normaliseEmail(email)
	if email == "" {
		return Operator{}, errors.New("creating an operator: no email address to sign in with")
	}
	if passwordHash == "" {
		return Operator{}, fmt.Errorf("creating the operator %s: no password hash to store", email)
	}

	stamp := now()
	operator := Operator{
		Email:        email,
		Name:         name,
		PasswordHash: passwordHash,
		IsOwner:      owner,
		CreatedAt:    stamp,
		UpdatedAt:    stamp,
	}

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO operators (email, name, password_hash, is_owner, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		operator.Email, operator.Name, operator.PasswordHash, operator.IsOwner,
		millis(stamp), millis(stamp),
	)
	if err != nil {
		// Both collisions raise the same extended result code and mean
		// different things to the caller, so they are told apart by the column
		// SQLite names in the message -- measured, a second row through the
		// partial unique index reported
		// "UNIQUE constraint failed: operators.is_owner (2067)" where a
		// duplicate address reported
		// "UNIQUE constraint failed: operators.email (2067)". Only the owner
		// path looks, so a message this driver one day rewords costs a specific
		// answer on one path and never a wrong one.
		if isUniqueViolation(err) {
			if owner && strings.Contains(err.Error(), "is_owner") {
				return Operator{}, fmt.Errorf("creating the owner %s: %w", email, ErrOwnerExists)
			}
			return Operator{}, fmt.Errorf("creating the operator %s: %w", email, ErrExists)
		}
		return Operator{}, fmt.Errorf("creating the operator %s: %w", email, err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return Operator{}, fmt.Errorf("reading back the id of the operator %s: %w", email, err)
	}
	operator.ID = id
	return operator, nil
}

// Operator reads one operator by id, or answers [ErrNotFound].
func (s *Store) Operator(ctx context.Context, id int64) (Operator, error) {
	operator, err := s.scanOperator(s.db.QueryRowContext(ctx,
		selectOperatorByID, id))
	if err != nil {
		return Operator{}, fmt.Errorf("reading the operator %d: %w", id, err)
	}
	return operator, nil
}

// OperatorByEmail reads the operator who signs in with this address, or answers
// [ErrNotFound]. The address is normalised first, so that an operator typing
// their address the way they feel like typing it today still finds their row.
func (s *Store) OperatorByEmail(ctx context.Context, email string) (Operator, error) {
	email = normaliseEmail(email)
	operator, err := s.scanOperator(s.db.QueryRowContext(ctx,
		selectOperatorByEmail, email))
	if err != nil {
		return Operator{}, fmt.Errorf("reading the operator %s: %w", email, err)
	}
	return operator, nil
}

// Owner reads the operator who may manage other operators, or answers
// [ErrNotFound] when this installation has not been set up yet.
//
// That [ErrNotFound] is the condition ADR-0007 requires before a Setup Token
// may be reissued. Without it the reissue command would be an unauthenticated
// path to owning the host.
func (s *Store) Owner(ctx context.Context) (Operator, error) {
	operator, err := s.scanOperator(s.db.QueryRowContext(ctx,
		selectOwner))
	if err != nil {
		return Operator{}, fmt.Errorf("reading this installation's owner: %w", err)
	}
	return operator, nil
}

// Operators reads every operator, owner first and then by the order they were
// created, which is the order the interface lists them in.
func (s *Store) Operators(ctx context.Context) ([]Operator, error) {
	rows, err := s.db.QueryContext(ctx,
		selectOperators)
	if err != nil {
		return nil, fmt.Errorf("listing the operators: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var operators []Operator
	for rows.Next() {
		operator, err := s.scanOperator(rows)
		if err != nil {
			return nil, fmt.Errorf("listing the operators: %w", err)
		}
		operators = append(operators, operator)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing the operators: %w", err)
	}
	return operators, nil
}

// SetPassword replaces an operator's password hash and ends every session they
// have, in one transaction.
//
// The two halves are one act and are not offered separately. A password is
// changed because it might be known to somebody else -- by the operator who
// suspects it, or by the Owner who is taking an account back -- and a change
// that leaves the existing sessions alive has not taken anything away from
// whoever knew it. Sessions are server-side (see the schema) precisely so that
// this is possible at all.
//
// It answers [ErrNotFound] when there is no such operator, and reports how many
// sessions it ended.
func (s *Store) SetPassword(ctx context.Context, id int64, passwordHash string) (revoked int64, err error) {
	if passwordHash == "" {
		return 0, fmt.Errorf("setting the password of the operator %d: no password hash to store", id)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("setting the password of the operator %d: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx,
		`UPDATE operators SET password_hash = ?, updated_at = ? WHERE id = ?`,
		passwordHash, millis(now()), id)
	if err != nil {
		return 0, fmt.Errorf("setting the password of the operator %d: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("setting the password of the operator %d: %w", id, err)
	}
	if changed == 0 {
		return 0, fmt.Errorf("setting the password of the operator %d: %w", id, ErrNotFound)
	}

	ended, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE operator_id = ?`, id)
	if err != nil {
		return 0, fmt.Errorf("ending the sessions of the operator %d: %w", id, err)
	}
	revoked, err = ended.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ending the sessions of the operator %d: %w", id, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("setting the password of the operator %d: %w", id, err)
	}
	return revoked, nil
}

// Rename changes what the interface calls an operator. It answers [ErrNotFound]
// when there is no such operator.
//
// There is no call that changes an email address. It is the identity an
// operator signs in with and it is what the audit trail recorded them as; an
// installation that wants a different one removes the operator and invites the
// new address.
func (s *Store) Rename(ctx context.Context, id int64, name string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE operators SET name = ?, updated_at = ? WHERE id = ?`, name, millis(now()), id)
	if err != nil {
		return fmt.Errorf("renaming the operator %d: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("renaming the operator %d: %w", id, err)
	}
	if changed == 0 {
		return fmt.Errorf("renaming the operator %d: %w", id, ErrNotFound)
	}
	return nil
}

// RemoveOperator removes an operator and, with them, every session they hold.
//
// The sessions go by ON DELETE CASCADE rather than by a second statement here,
// so that access cannot survive the account even if this code is one day
// changed carelessly. Their audit entries stay: the trail records the email
// address as it was, and the row's link to the operator becomes NULL.
//
// It refuses the Owner with [ErrOwnerProtected]. The refusal is a WHERE clause
// rather than a read followed by a delete, so there is no window in which the
// row could become the owner in between. It answers [ErrNotFound] when there is
// no such operator at all -- the two are told apart by asking afterwards, which
// is safe because by then the row either exists and is the owner, or does not.
func (s *Store) RemoveOperator(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM operators WHERE id = ? AND NOT is_owner`, id)
	if err != nil {
		return fmt.Errorf("removing the operator %d: %w", id, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("removing the operator %d: %w", id, err)
	}
	if removed > 0 {
		return nil
	}

	var isOwner bool
	switch err := s.db.QueryRowContext(ctx, `SELECT is_owner FROM operators WHERE id = ?`, id).Scan(&isOwner); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("removing the operator %d: %w", id, ErrNotFound)
	case err != nil:
		return fmt.Errorf("removing the operator %d: %w", id, err)
	default:
		return fmt.Errorf("removing the operator %d: %w", id, ErrOwnerProtected)
	}
}

// row is what QueryRow and Rows have in common, so that one scan serves both.
type row interface {
	Scan(dest ...any) error
}

func (s *Store) scanOperator(r row) (Operator, error) {
	var (
		operator             Operator
		createdAt, updatedAt int64
	)
	err := r.Scan(&operator.ID, &operator.Email, &operator.Name, &operator.PasswordHash,
		&operator.IsOwner, &createdAt, &updatedAt)
	if err != nil {
		return Operator{}, notFound(err)
	}
	operator.CreatedAt, operator.UpdatedAt = instant(createdAt), instant(updatedAt)
	return operator, nil
}

// normaliseEmail is what makes "one operator per address" mean what an operator
// expects it to mean.
//
// Lower-cased and trimmed, then stored that way, so the unique index is over
// the normalised form and Owner@example.com cannot be invited twice by shifting
// a key. The domain part is case-insensitive by RFC 1035 and the local part is
// formally case-sensitive; treating it as sensitive would mean an operator who
// capitalises their own address at the sign-in box is told they do not exist,
// which is worse than the theoretical address it excludes.
func normaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
