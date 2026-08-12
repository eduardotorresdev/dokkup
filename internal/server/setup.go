package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/secret"
	"github.com/eduardotorresdev/dokkup/internal/store"
)

// The audit vocabulary this route writes. The strings are the store's
// contract, not this package's invention -- see internal/store/audit.go -- and
// they are named here so that a typo is a compile error in one place rather
// than an entry nothing will ever query for.
const (
	auditSetupRedeemed = "setup-token.redeemed"
	auditSetupRejected = "setup-token.rejected"
)

// What a redemption attempt is allowed to be.
const (
	// setupBodyLimit bounds what is read before any of it is parsed. The
	// largest legitimate body is a token, a name, an address and a 1024-byte
	// password; 8 KiB is generous for that and stops an unauthenticated caller
	// streaming megabytes into a JSON decoder.
	setupBodyLimit = 8 << 10

	// minPasswordBytes is the floor NIST SP 800-63B puts under a
	// user-chosen secret, and it is the only rule dokkup imposes: composition
	// rules push people towards "Password1!" and are not checked here.
	minPasswordBytes = 12

	// maxPasswordBytes exists because argon2id is deliberately expensive and
	// its cost grows with the input. Without a ceiling, the one unauthenticated
	// mutation dokkup has would hash whatever it was sent, at 64 MiB a time --
	// a denial of service with a submit button, which is the same argument the
	// cost constants in internal/secret make. The check runs before the hash,
	// which is the only place it can do any good.
	maxPasswordBytes = 1024

	// maxEmailBytes is the length of an address RFC 5321 allows. It is
	// enforced because a rejected attempt's address goes into the audit trail,
	// which is kept forever: without a bound, anyone could write eight
	// kilobytes into it five times a minute.
	maxEmailBytes = 320
)

// The one refusal that must not vary. Unknown, expired and already spent are
// one answer and one string, because a caller who can tell them apart learns
// that a token they hold was once real, and an attacker who can tell "expired"
// from "unknown" has an oracle for guessing.
const setupTokenRefusal = "that setup token is not valid"

// setupOwnerRefusal reads the same as [store.ErrOwnerExists] and is written out
// again rather than taken from it. What crosses the wire is part of the API and
// what an error value says is not: an edit to the store's wording must not
// quietly change what a browser is shown.
const setupOwnerRefusal = "this installation already has an owner"

// How often a redemption may be attempted: five per minute, starting full.
//
// Five is enough for an operator mistyping a token pasted out of a terminal and
// far below what guessing a 256-bit secret would need. The window is per
// process and shared by everybody, for the reason [tokenBucket] gives.
const (
	setupBurst  = 5
	setupRefill = time.Minute / setupBurst
)

// setupRequest is what the browser posts. It is a local type and not a shared
// one: nothing else in dokkup takes a password over the wire, and a struct
// reused by a second route is a struct whose fields grow optional.
type setupRequest struct {
	Token    string `json:"token"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// handleSetup redeems the Setup Token and creates the Owner. It is the only
// unauthenticated mutation dokkup has, and ADR-0007 is the reason it is allowed
// to exist at all: without it the first visitor to reach the port would own the
// machine.
//
// Everything below is ordered by what it costs and what it reveals. The rate
// limit comes first so that a flood is refused before it is parsed; the
// password ceiling comes before the hash so that a huge password is refused
// before it is paid for; the token is spent last, by the store, in the
// transaction that creates the Owner.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	// Taken before anything else, and taken by attempts that go on to fail:
	// failed attempts are exactly what this counter exists against, and one
	// that only counted successes would let an attacker guess without limit.
	if allowed, wait := s.setupLimiter.take(); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
		writeJSON(w, http.StatusTooManyRequests, errorBody("too many attempts"))
		// Deliberately not audited. A refusal here is unbounded -- the limiter
		// stops the work, not the requests -- so recording one would let
		// anybody grow the trail as fast as they can open sockets, and the
		// trail is the file an operator reads to find out what happened.
		return
	}

	if s.cfg.Store == nil {
		// Nothing to write the Owner into, and nothing to audit the refusal
		// into either. It is a 503 and not a 500 because a dokkup started
		// without a database is misconfigured rather than broken, and saying so
		// is what tells the operator which of the two to fix.
		writeJSON(w, http.StatusServiceUnavailable, errorBody("this dokkup has no database"))
		return
	}

	// The trail is written even when the caller has already hung up: a rejected
	// redemption is a security event, and an attacker who closes the connection
	// the moment they are refused must not thereby erase the record of it.
	audit := context.WithoutCancel(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, setupBodyLimit)

	var body setupRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// The decoder's error quotes the input it choked on, and the input is a
		// token and a password. It is dropped here rather than logged or
		// returned, which is why this branch says only that the body was bad.
		s.rejectSetup(audit, w, http.StatusBadRequest, "that request body is not valid json", "")
		return
	}

	email, invalid := validateSetup(body)
	if invalid != nil {
		s.rejectSetup(audit, w, http.StatusBadRequest, invalid.message, invalid.target)
		return
	}

	// Asked before the token is looked at, so that an installation which is
	// already set up answers "there is an owner" rather than "that token is not
	// valid" -- the honest answer, and one that reveals nothing new, because
	// /api/session already tells every anonymous caller whether setup is done.
	// The store answers it a second time, inside the redeeming transaction,
	// because a check here and a write later is a race and only the transaction
	// closes it; see the ErrOwnerExists branch below.
	owned, err := s.cfg.Store.HasOwner(r.Context())
	if err != nil {
		s.logger.Error("reading whether this installation has an owner", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("this dokkup could not read its own database"))
		return
	}
	if owned {
		s.rejectSetup(audit, w, http.StatusConflict, setupOwnerRefusal, email)
		return
	}

	passwordHash, err := secret.HashPassword(body.Password)
	if err != nil {
		s.logger.Error("hashing the owner's password", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("this dokkup could not hash that password"))
		return
	}

	// The token is hashed here and the token itself goes no further: the store
	// is given the hash, and the variable holding the secret is never logged,
	// recorded or echoed.
	owner, err := s.cfg.Store.RedeemSetupToken(r.Context(),
		secret.HashToken(body.Token), email, body.Name, passwordHash)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Unknown, expired and already spent arrive here as one error, from one
		// statement, and leave as one response. There is no branch to tell them
		// apart even if a future reader wanted one.
		s.rejectSetup(audit, w, http.StatusForbidden, setupTokenRefusal, email)
		return
	case errors.Is(err, store.ErrOwnerExists), errors.Is(err, store.ErrExists):
		// ErrOwnerExists is the race the check above cannot close: two
		// redemptions in flight, and this is the loser. ErrExists is a
		// different thing collapsed into the same answer on purpose -- it is a
		// duplicate email address, which is reachable when an Owner was removed
		// and an ordinary operator kept that address. The reply then says
		// something untrue, and it stays that way deliberately: telling the two
		// apart would turn this unauthenticated route into a way of asking
		// whether a given address has an account here. Do not "fix" the branch
		// by splitting it; the enumeration is the bug, the vague answer is not.
		s.rejectSetup(audit, w, http.StatusConflict, setupOwnerRefusal, email)
		return
	case err != nil:
		// The store's errors name the email address and never the token, the
		// hash or the password -- see internal/store/setuptoken.go, which is
		// explicit about it -- so this one may be logged.
		s.logger.Error("redeeming the setup token", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("this dokkup could not create the owner"))
		return
	}

	// Attributed to the Owner who was just created, which is the whole reason
	// this entry is written after the redemption rather than before it: there
	// was nobody to attribute it to until the transaction committed.
	if err := s.cfg.Store.Record(audit, store.Audited{
		OperatorID: owner.ID,
		Action:     auditSetupRedeemed,
		Target:     owner.Email,
	}); err != nil {
		// Logged and not returned. The Owner exists and the token is spent;
		// answering with a failure would tell the operator to try again with a
		// credential that no longer works, which is worse than a gap in the
		// trail that this log line fills.
		s.logger.Error("recording that the setup token was redeemed", "error", err)
	}

	// No session cookie. Redeeming the token creates the Owner and nothing
	// else; they sign in afterwards, through the one route that starts a
	// session. Minting one here would be a second way to become authenticated,
	// with its own copy of the cookie's flags to keep in step.
	writeJSON(w, http.StatusCreated, map[string]any{"operator": operatorBody(owner)})
}

// invalidSetup is what is wrong with a request, in the two parts answering it
// takes: what to tell the caller, and what address to record the attempt
// against. A nil one means nothing is wrong.
type invalidSetup struct {
	message string

	// target is empty when the request carried no address worth recording --
	// there is nothing useful to write down about an attempt whose address is
	// the thing that was malformed.
	target string
}

// validateSetup normalises a request and reports the first thing wrong with it.
//
// It is a function of the body alone -- no writer, no store, no logger -- so
// that the rules can be read and tested without a server, and so that
// [Server.handleSetup] reads as the order it cares about: refuse a flood,
// refuse without a database, parse, validate, refuse a claimed host, hash, and
// only then spend the token.
//
// The address comes back normalised because everything downstream must agree on
// it: what the store matches, what the trail records, and what the response
// echoes are one string or they are a bug.
func validateSetup(body setupRequest) (string, *invalidSetup) {
	email := strings.ToLower(strings.TrimSpace(body.Email))

	// Lengths in bytes, not runes: what the ceilings protect -- the audit
	// trail's size and argon2's working set -- is measured in bytes, and
	// counting runes would let a caller past both with the same allocation.
	switch {
	case email == "":
		return email, &invalidSetup{message: "an email address is required"}
	case len(email) > maxEmailBytes:
		return email, &invalidSetup{message: "that email address is too long"}
	case len(body.Password) < minPasswordBytes:
		return email, &invalidSetup{
			message: fmt.Sprintf("a password must be at least %d characters", minPasswordBytes),
			target:  email,
		}
	case len(body.Password) > maxPasswordBytes:
		return email, &invalidSetup{message: "that password is too long", target: email}
	}
	return email, nil
}

// rejectSetup answers a failed attempt and records it, in that order of
// importance and the opposite order of operations.
//
// Every refusal of the *caller* goes through here so that no path can forget
// the trail: an unauthenticated route that creates the Owner is the one place
// where the attempts that failed are more interesting than the one that worked.
// What goes in Target is the address the attempt claimed, which is a claim and
// not an identity -- [store.Store.RecordUnauthenticated] is the call that says
// so. The token, its hash, the password and its hash are never passed in.
//
// The exceptions are dokkup's own failures, the three 500s and the 503, which
// answer with [writeJSON] directly and are logged instead. They are not
// oversights: every one of them is reached because the database could not be
// read, written to or opened, and the trail lives in that database. A refusal
// that tried to audit itself there would either fail a second time or hide the
// first failure behind the second.
func (s *Server) rejectSetup(ctx context.Context, w http.ResponseWriter, status int, message, email string) {
	if err := s.cfg.Store.RecordUnauthenticated(ctx, store.Audited{
		Action: auditSetupRejected,
		Target: email,
	}); err != nil {
		s.logger.Error("recording a rejected setup token redemption", "error", err)
	}
	writeJSON(w, status, errorBody(message))
}

// errorBody is the one shape every failure takes, because the client reads
// exactly one field. See web/src/lib/api.ts.
func errorBody(message string) map[string]any { return map[string]any{"error": message} }

// methodNotAllowed answers everything that is not a POST to a route that only
// accepts one.
//
// It exists because the catch-all route serves the single-page application, so
// without a pattern matching the bare path a GET to one of these would be
// answered with the frontend shell and a 200 -- which is what a client with a
// bug would read as success. The message is a parameter rather than one shared
// sentence because it is what an operator sees in a browser's network tab, and
// "signing in is done with POST" is worth more there than "method not allowed".
func methodNotAllowed(message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, errorBody(message))
	}
}
