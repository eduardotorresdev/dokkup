package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/secret"
	"github.com/eduardotorresdev/dokkup/internal/store"
)

// The audit vocabulary the two session routes write. Named here for the reason
// the setup constants are named in setup.go: a typo has to be a compile error
// in one place rather than an entry nothing will ever query for.
const (
	auditSessionStarted  = "session.started"
	auditSessionEnded    = "session.ended"
	auditSessionRejected = "session.rejected"
)

// sessionCookieName is what the browser holds a session in.
//
// A cookie and never a value in local storage: injected script can read
// storage and cannot read an HttpOnly cookie, and every operator here is
// root-equivalent on the Dokku Host, so a readable session token is a readable
// root account. web/src/lib/api.ts already assumes this and sends no
// Authorization header.
const sessionCookieName = "dokkup_session"

// sessionLifetime is how long a sign-in lasts, absolutely.
//
// There is no sliding renewal. Renewing on use means a session that is used
// once a day never expires, so the only thing that ever ends it is somebody
// remembering to sign out -- and a stolen cookie renews itself just as happily
// as its owner does. Seven days is short enough that a forgotten browser stops
// mattering within a week and long enough that an operator is not typing a
// password into a deployment screen every morning.
const sessionLifetime = 7 * 24 * time.Hour

// What a sign-in attempt is allowed to be.
const (
	// signinBodyLimit bounds what is read before any of it is parsed. The
	// largest legitimate body here is an address and a 1024-byte password, so
	// this is smaller than [setupBodyLimit]: sign-in carries no token and no
	// name, and the ceiling should be the shape of the route rather than a
	// number copied from the one next to it.
	signinBodyLimit = 4 << 10

	// How often a sign-in may be attempted: ten per minute, starting full.
	//
	// Twice [setupBurst], because the two routes are used at different rates.
	// Setup happens once per installation and five attempts is generous for a
	// pasted token; sign-in is the screen an operator meets every week, on a
	// phone, with a password manager that sometimes fills the wrong entry.
	//
	// The bucket is global to the process, for the reason [tokenBucket] gives
	// at length: dokkup sits behind nginx, so the address this process sees is
	// the proxy's or a header an attacker sets, and neither is an identity. The
	// price of that, stated plainly because it is real and it is not fixed
	// here: a stranger sending ten requests a minute keeps this bucket empty,
	// and the Owner meets 429 on their own host for as long as the stranger
	// cares to go on sending. The window does not expire the attacker -- it
	// refills, they spend it again, indefinitely. What the limit buys is that
	// guessing runs at ten attempts a minute against argon2id rather than as
	// fast as sockets open; what it does not buy is availability under a
	// deliberate flood.
	//
	// The two obvious repairs are both worse and neither belongs to this
	// route's design: a bucket per identity lets anyone lock out a known email
	// address, and a bucket per client address is the thing [tokenBucket]
	// argues against. Fixing it properly is somebody's issue, not a constant
	// to be nudged here.
	signinBurst  = 10
	signinRefill = time.Minute / signinBurst
)

// signinRefusal is the one answer every refused sign-in gets.
//
// Unknown address, wrong password and "the credentials were right but this
// installation is in IP Mode and you are not the Owner" are one string, one
// status and one code path. A caller who can tell them apart can enumerate the
// operators of a host, and the IP-Mode variant is worse still: a distinct
// answer there would confirm that the password just typed was correct.
// /api/session already publishes ownerOnly, so the frontend needs no help from
// this route, and the audit trail is where the real reason is written down.
const signinRefusal = "that email address and password do not match"

// notSignedIn is what [Server.requireSession] answers with. It is deliberately
// the same sentence whether there was no cookie, a cookie naming a session that
// expired, or one naming a session that was revoked: none of those is a state
// the caller can do anything different about, and telling them apart would say
// whether a token was ever real.
const notSignedIn = "you are not signed in"

// signinRequest is what the browser posts. A local type, for the reason
// [setupRequest] is one: nothing else in dokkup takes a password over the wire,
// and a struct reused by a second route is a struct whose fields grow optional.
type signinRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// dummyPasswordHash returns a hash no password verifies against, so that a
// sign-in for an address that does not exist still pays for an argon2id
// verification.
//
// Without it, an unknown operator answers in microseconds and a wrong password
// answers in the tens of milliseconds the cost constants in internal/secret
// were chosen for, and the difference is a clock an attacker reads the operator
// list off. The refusals are already one string; this is what makes them one
// duration as well.
//
// It is computed once, lazily, from a fresh random string rather than being
// pasted in as a literal: a literal in source is a hash nobody can check the
// provenance of, and one built here is by construction a real
// [secret.HashPassword] output at today's cost -- which is the point, since
// raising that cost must move the dummy with it. Lazily because 64 MiB of
// argon2 at process start would be paid by every `dokkup install` and `dokkup
// update` that never serves a request.
var dummyPasswordHash = sync.OnceValue(func() string {
	// The token is only ever a source of randomness here; it is hashed as a
	// password and then dropped, and no caller can supply it.
	filler, _, err := secret.NewToken()
	if err != nil {
		return ""
	}
	hash, err := secret.HashPassword(filler)
	if err != nil {
		// An empty hash fails to parse, so [secret.VerifyPassword] answers
		// false quickly and the timing equalisation is lost -- but the refusal
		// itself is not, and the only way to reach this branch is crypto/rand
		// or argon2 failing, at which point dokkup has larger problems than a
		// side channel.
		return ""
	}
	return hash
})

// handleSignIn exchanges an address and a password for a session cookie.
//
// The order is the one [Server.handleSetup] argues for and for the same
// reasons: refuse a flood before parsing it, refuse without a database, bound
// the body before decoding it, bound the password before hashing it, and only
// then spend anything expensive. What is different here is that every refusal
// after the parse is the same refusal -- see [signinRefusal].
func (s *Server) handleSignIn(w http.ResponseWriter, r *http.Request) {
	// Taken by attempts that go on to fail, because failed attempts are exactly
	// what this counter exists against.
	if allowed, wait := s.signinLimiter.take(); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
		writeJSON(w, http.StatusTooManyRequests, errorBody("too many attempts"))
		// Not audited, for the reason handleSetup gives: a refusal here is
		// unbounded, so recording one would let anybody grow the trail as fast
		// as they can open sockets.
		return
	}

	if s.cfg.Store == nil {
		// Nowhere to read an operator from and nowhere to write the trail
		// either. A 503 rather than a 500 because a dokkup started without a
		// database is misconfigured rather than broken.
		writeJSON(w, http.StatusServiceUnavailable, errorBody("this dokkup has no database"))
		return
	}

	// The trail survives a caller who hangs up. A refused sign-in is a security
	// event, and an attacker who closes the connection the moment they are
	// refused must not thereby erase the record of it.
	audit := context.WithoutCancel(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, signinBodyLimit)

	var body signinRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// The decoder's error quotes the input it choked on, and the input is a
		// password. It is dropped rather than logged or returned.
		//
		// Recorded but answered separately: this is the one refusal that is not
		// a refusal of anybody's credentials, and answering it with
		// [signinRefusal] would tell a client with a bug that its password is
		// wrong when what is wrong is its JSON.
		s.recordRejectedSignIn(audit, store.Audited{Action: auditSessionRejected})
		writeJSON(w, http.StatusBadRequest, errorBody("that request body is not valid json"))
		return
	}

	// Normalised the way the store matches and the way the trail records, so
	// that what is looked up, what is written down and what would be echoed are
	// one string.
	email := strings.ToLower(strings.TrimSpace(body.Email))
	rejected := store.Audited{Action: auditSessionRejected, Target: email}

	// Checked before the hash, which is the only place the check does any good,
	// and answered with the ordinary refusal rather than with a message about
	// length: no password this dokkup ever stored is longer than
	// [maxPasswordBytes], so an over-long one cannot be a password that would
	// have matched, and a distinct answer would only tell a caller which of
	// their inputs the server bothered to look at. The address is bounded for
	// the reason [maxEmailBytes] exists -- it is what goes into a trail that is
	// kept forever.
	if email == "" || len(email) > maxEmailBytes || len(body.Password) > maxPasswordBytes {
		if len(email) > maxEmailBytes {
			// Nothing worth writing down about an attempt whose address is the
			// thing that was malformed, and writing it down is the abuse.
			rejected.Target = ""
		}
		s.rejectSignIn(audit, w, rejected)
		return
	}

	operator, err := s.cfg.Store.OperatorByEmail(r.Context(), email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// The cost is paid anyway. See [dummyPasswordHash]; the result is
		// discarded because it is false by construction.
		_, _ = s.verifyPassword(dummyPasswordHash(), body.Password)
		s.rejectSignIn(audit, w, rejected)
		return
	case err != nil:
		// The store's errors name the address and never a credential -- see
		// internal/store/operator.go -- so this one may be logged.
		s.logger.Error("reading the operator signing in", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("this dokkup could not read its own database"))
		return
	}

	matches, err := s.verifyPassword(operator.PasswordHash, body.Password)
	if err != nil {
		// A stored hash this dokkup cannot parse is a broken row, and it is
		// still not a password that matched. It is logged and answered with the
		// ordinary refusal rather than a 500, because a 500 here happens only
		// for an address that exists: the honest-looking answer would be an
		// enumeration oracle. The error from internal/secret never carries the
		// password or the hash.
		s.logger.Error("verifying an operator's password", "error", err)
		s.rejectSignIn(audit, w, rejected)
		return
	}
	if !matches {
		s.rejectSignIn(audit, w, rejected)
		return
	}

	// IP Mode restricts dokkup to the Owner, because a host reached by IP has
	// no certificate a browser trusts and therefore no confidentiality worth
	// handing to a second person (ADR-0006). The credentials were correct, so
	// this is the one refusal that knows who it refused, and it is attributed
	// to them: the response is byte-identical to every other refusal, and the
	// trail is the one place the difference is allowed to show.
	if s.cfg.Mode.Restricted() && !operator.IsOwner {
		rejected.OperatorID = operator.ID
		s.rejectSignIn(audit, w, rejected)
		return
	}

	// The token is the browser's copy and the hash is the store's. They are
	// produced together so that no code path holds a token it could log; see
	// [secret.NewToken].
	token, hash, err := secret.NewToken()
	if err != nil {
		s.logger.Error("drawing a session token", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("this dokkup could not start a session"))
		return
	}

	expires := time.Now().Add(sessionLifetime)
	if _, err := s.cfg.Store.StartSession(r.Context(), operator.ID, hash, expires); err != nil {
		s.logger.Error("starting a session", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("this dokkup could not start a session"))
		return
	}

	http.SetCookie(w, s.sessionCookie(token, expires))

	if err := s.cfg.Store.Record(audit, store.Audited{
		OperatorID: operator.ID,
		Action:     auditSessionStarted,
		Target:     operator.Email,
	}); err != nil {
		// Logged and not returned. The session exists and the cookie is already
		// on the response; answering with a failure would tell an operator that
		// a sign-in which worked did not.
		s.logger.Error("recording that a session started", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"operator": operatorBody(operator)})
}

// rejectSignIn answers a refused sign-in and records it.
//
// One function so that no path can invent a second wording or a second status:
// every refusal of the credentials is [signinRefusal] with a 401, and the only
// thing that varies between them is what goes into the trail.
//
// The password, the address's hash and the session token are never passed in.
// What goes in Target is the address the attempt claimed, which is a claim and
// not an identity except in the one case that verified it -- see the IP-Mode
// branch above, which is why this takes an [store.Audited] rather than a string.
func (s *Server) rejectSignIn(ctx context.Context, w http.ResponseWriter, entry store.Audited) {
	s.recordRejectedSignIn(ctx, entry)
	writeJSON(w, http.StatusUnauthorized, errorBody(signinRefusal))
}

// recordRejectedSignIn writes the trail entry a refusal leaves behind.
//
// [store.Store.Record] and [store.Store.RecordUnauthenticated] are two calls
// rather than one taking a nil operator, because an unattributed entry has to
// be a decision somebody wrote down; this is where that decision is made, and
// there is exactly one attempt that can name an operator -- the IP-Mode
// refusal, which verified the password before refusing it.
//
// A failure to record is logged and not returned. The caller is being refused
// either way, and turning a gap in the trail into a different answer would be a
// second thing for an attacker to provoke.
func (s *Server) recordRejectedSignIn(ctx context.Context, entry store.Audited) {
	var err error
	if entry.OperatorID != 0 {
		err = s.cfg.Store.Record(ctx, entry)
	} else {
		err = s.cfg.Store.RecordUnauthenticated(ctx, entry)
	}
	if err != nil {
		s.logger.Error("recording a refused sign-in", "error", err)
	}
}

// handleSignOut ends the session server-side and clears the cookie, in that
// order of importance.
//
// A sign-out that only cleared the cookie would be a promise the client keeps:
// the token would go on authenticating anybody who copied it out of the browser
// before it was dropped. The row is DELETEd, so a replay of the old cookie is a
// 401 from [Server.requireSession].
func (s *Server) handleSignOut(w http.ResponseWriter, r *http.Request) {
	// The middleware refused everybody else, so there is an operator here and
	// there is a store: no session exists without one.
	operator, ok := operatorFrom(r.Context())
	if !ok {
		// Unreachable through the middleware chain. It is a 401 rather than a
		// panic so that a future route table which forgot to guard this fails
		// closed.
		writeJSON(w, http.StatusUnauthorized, errorBody(notSignedIn))
		return
	}

	audit := context.WithoutCancel(r.Context())

	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if err := s.cfg.Store.RevokeSession(r.Context(), secret.HashToken(cookie.Value)); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			// The one failure worth stopping for: the row is still there, so
			// the token still authenticates, and clearing the cookie now would
			// tell the operator they are signed out while they are not.
			s.logger.Error("ending a session", "error", err)
			writeJSON(w, http.StatusInternalServerError, errorBody("this dokkup could not end that session"))
			return
		}
	}

	// Same name, path and flags as the cookie that was set: a browser matches
	// on all three, and one that differs leaves the original in place.
	http.SetCookie(w, s.clearedSessionCookie())

	if err := s.cfg.Store.Record(audit, store.Audited{
		OperatorID: operator.ID,
		Action:     auditSessionEnded,
		Target:     operator.Email,
	}); err != nil {
		s.logger.Error("recording that a session ended", "error", err)
	}

	w.WriteHeader(http.StatusNoContent)
}

// sessionCookie is the one place the cookie's shape is decided, so that setting
// it and clearing it cannot drift apart.
//
// HttpOnly keeps it away from script. SameSite=Strict means a browser never
// attaches it to a request another site started, which is what makes the CSRF
// header a second factor rather than the only one. Secure is on unless this
// dokkup was told it is reached over plain HTTP -- see [Config.PlainHTTP]: the
// flag is inverted rather than opt-in so that forgetting to pass anything
// leaves the cookie protected.
func (s *Server) sessionCookie(token string, expires time.Time) *http.Cookie {
	// gosec's G124 wants Secure to be a literal true and cannot read a field
	// that is false on exactly one kind of installation. The alternative it
	// would accept -- always Secure -- is the bug this method exists to avoid:
	// a browser silently drops a Secure cookie on a plain-HTTP origin, so
	// obeying the linter would make sign-in fail on those hosts with nothing
	// anywhere to say why. HttpOnly and SameSite are unconditional.
	return &http.Cookie{ //nolint:gosec
		Name:  sessionCookieName,
		Value: token,
		Path:  "/",
		// Whole seconds, matching the row's expires_at, so the browser stops
		// sending a cookie at about the moment the store stops honouring it.
		// Expiry is enforced in SQL either way -- see
		// [store.Store.SessionOperator] -- and this only saves a round trip.
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   !s.cfg.PlainHTTP,
		SameSite: http.SameSiteStrictMode,
	}
}

// clearedSessionCookie is what sign-out sends: the same cookie, emptied.
//
// MaxAge is -1 because that is how net/http spells "Max-Age=0", which is how a
// browser is told to drop a cookie now. Zero would mean "say nothing about the
// lifetime", which leaves the cookie in place.
func (s *Server) clearedSessionCookie() *http.Cookie {
	// Flagged by G124 for the reason [Server.sessionCookie] is: the flags are
	// decided there, once, which is the whole point of going through it.
	cookie := s.sessionCookie("", time.Time{}) //nolint:gosec
	cookie.MaxAge = -1
	return cookie
}

// operatorContextKey is the key the authenticated operator is carried under. A
// struct type of its own, unexported, so that nothing outside this package can
// construct the key and therefore nothing outside it can plant an operator on a
// request context.
type operatorContextKey struct{}

// operatorFrom reports which operator a request was authenticated as.
//
// The second return is false for a request that carried no session, which is
// only ever the allowlisted routes: everything else was refused before reaching
// a handler. It is the only way to read the operator, so a handler that forgets
// to check the boolean has a compile-shaped reminder rather than a zero
// [store.Operator] that looks like a real one.
func operatorFrom(ctx context.Context) (store.Operator, bool) {
	operator, ok := ctx.Value(operatorContextKey{}).(store.Operator)
	return operator, ok
}

// withOperator puts the authenticated operator on the context.
func withOperator(ctx context.Context, operator store.Operator) context.Context {
	return context.WithValue(ctx, operatorContextKey{}, operator)
}

// operatorBody is the one shape an operator takes on the wire, shared by
// /api/setup, /api/signin and /api/session. See the Operator interface in
// web/src/lib/api.ts, which is one type for all three.
//
// The password hash is not a field here and must never become one.
func operatorBody(operator store.Operator) map[string]any {
	return map[string]any{
		"email":   operator.Email,
		"name":    operator.Name,
		"isOwner": operator.IsOwner,
	}
}
