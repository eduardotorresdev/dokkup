package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/secret"
	"github.com/eduardotorresdev/dokkup/internal/store"
)

// These tests are inside the package for the reason the setup ones are: the
// sign-in rate limit is per minute, and the clock it reads is a field on a
// bucket only this package can reach. They also register a route of their own
// on the mux, which is the only way to test "a route added later is protected"
// without the assertion decaying into a list of today's routes.

// probePath is a route that exists only while a test is running. Nothing
// allowlists it, and nothing ever should: it stands in for the routes issue #16
// and everything after it will add, and the property under test is that such a
// route is refused to a stranger without anybody having remembered to say so.
const probePath = "/api/probe"

// newAuthTest builds a server on a fresh database, with a clock the test owns
// and a probe route registered on the mux behind the ordinary middleware chain.
func newAuthTest(t *testing.T, cfg Config) (*httptest.Server, *store.Store, *fakeClock) {
	t.Helper()

	s, st, clock := newAuthServer(t, cfg)

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, st, clock
}

// newAuthServer is the same, stopping one step short: it hands back the server
// itself, for the one test that has to watch a call the response cannot show.
func newAuthServer(t *testing.T, cfg Config) (*Server, *store.Store, *fakeClock) {
	t.Helper()

	st := openTestStore(t)
	cfg.Store = st
	// Discarded rather than default, because these tests provoke the error
	// paths on purpose and a passing run must not look like a failing one.
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	s := New(cfg)

	clock := &fakeClock{now: time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)}
	s.signinLimiter = newTokenBucket(signinBurst, signinRefill, clock.Now)

	// Registered after New and therefore after routes(), which is the whole
	// point: this is a route the allowlist has never heard of.
	s.mux.HandleFunc("GET "+probePath, func(w http.ResponseWriter, r *http.Request) {
		operator, ok := operatorFrom(r.Context())
		if !ok {
			// Reaching here at all is the bug this route exists to catch: the
			// middleware must have refused the request before the handler.
			writeJSON(w, http.StatusInternalServerError, errorBody("no operator on the context"))
			return
		}
		writeJSON(w, http.StatusOK, operatorBody(operator))
	})
	s.mux.HandleFunc("POST "+probePath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"mutated": true})
	})

	return s, st, clock
}

// do performs one request, carrying whatever cookies the caller holds, and
// reads the whole body. The cookies are explicit rather than kept in a jar
// because the session cookie is Secure by default and a jar would refuse to
// store it over the plain HTTP a test server speaks -- which would make every
// assertion below pass or fail for the wrong reason.
func do(t *testing.T, srv *httptest.Server, method, path, body string, csrf bool, cookies ...*http.Cookie) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set(csrfHeader, "1")
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	read, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response to %s %s: %v", method, path, err)
	}
	return resp, read
}

func signinJSON(t *testing.T, email, password string) string {
	t.Helper()

	body, err := json.Marshal(signinRequest{Email: email, Password: password})
	if err != nil {
		t.Fatalf("encoding a sign-in request: %v", err)
	}
	return string(body)
}

// makeOwner creates the Owner the way redeeming a Setup Token would have,
// through the same hashing the server uses, so that a sign-in test exercises
// [secret.VerifyPassword] against a hash nothing in the test wrote by hand.
func makeOwner(t *testing.T, st *store.Store, email, password string) store.Operator {
	t.Helper()
	return makeOperator(t, st, email, password, true)
}

func makeOperator(t *testing.T, st *store.Store, email, password string, owner bool) store.Operator {
	t.Helper()

	hash, err := secret.HashPassword(password)
	if err != nil {
		t.Fatalf("hashing a password: %v", err)
	}

	var operator store.Operator
	if owner {
		operator, err = st.CreateOwner(t.Context(), email, "Ada", hash)
	} else {
		operator, err = st.CreateOperator(t.Context(), email, "Grace", hash)
	}
	if err != nil {
		t.Fatalf("creating an operator: %v", err)
	}
	return operator
}

// sessionCookieOf returns the session cookie a response set, or fails.
func sessionCookieOf(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()

	for _, cookie := range resp.Cookies() {
		if cookie.Name == sessionCookieName {
			return cookie
		}
	}
	t.Fatalf("no %s cookie on the response", sessionCookieName)
	return nil
}

// signIn signs the Owner in and hands back the cookie a browser would hold.
func signIn(t *testing.T, srv *httptest.Server, email, password string) *http.Cookie {
	t.Helper()

	resp, raw := do(t, srv, http.MethodPost, "/api/signin", signinJSON(t, email, password), true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in status = %d, want %d: %s", resp.StatusCode, http.StatusOK, raw)
	}
	return sessionCookieOf(t, resp)
}

func TestSigningInStartsASessionThatWorks(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModePublished})
	owner := makeOwner(t, st, goodEmail, goodPassword)

	// Typed the way a browser's autofill would leave it, because the address
	// that signs in and the address the store matches must be one string.
	resp, raw := do(t, srv, http.MethodPost, "/api/signin",
		signinJSON(t, " Ops@Example.com ", goodPassword), true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusOK, raw)
	}

	var body struct {
		Operator struct {
			Email   string `json:"email"`
			Name    string `json:"name"`
			IsOwner bool   `json:"isOwner"`
		} `json:"operator"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if body.Operator.Email != goodEmail || body.Operator.Name != "Ada" || !body.Operator.IsOwner {
		t.Errorf("operator = %+v, want %s/Ada and the owner", body.Operator, goodEmail)
	}

	cookie := sessionCookieOf(t, resp)

	// The row is what a session is. A cookie whose token nothing stored would
	// authenticate nobody, and would fail the request below for a reason no
	// assertion here would name.
	session, err := st.Session(t.Context(), secret.HashToken(cookie.Value))
	if err != nil {
		t.Fatalf("reading the session back: %v", err)
	}
	if session.OperatorID != owner.ID {
		t.Errorf("session belongs to operator %d, want %d", session.OperatorID, owner.ID)
	}

	// Absolute, not sliding: a lifetime that renewed on use would never end.
	if want := time.Now().Add(sessionLifetime); session.ExpiresAt.Sub(want).Abs() > time.Minute {
		t.Errorf("session expires at %s, want about %s", session.ExpiresAt, want)
	}

	resp, raw = do(t, srv, http.MethodGet, probePath, "", false, cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the session does not open a guarded route: status = %d: %s", resp.StatusCode, raw)
	}
}

// The cookie's flags are the whole of what keeps a session out of reach of
// injected script and of another site, so each one is asserted rather than
// assumed.
func TestTheSessionCookieIsHttpOnlyStrictAndSecure(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModePublished})
	makeOwner(t, st, goodEmail, goodPassword)

	cookie := signIn(t, srv, goodEmail, goodPassword)

	if !cookie.HttpOnly {
		t.Error("the session cookie is readable by script")
	}
	if !cookie.Secure {
		t.Error("the session cookie is sent over plain HTTP")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("Path = %q, want /", cookie.Path)
	}
	if cookie.MaxAge <= 0 {
		t.Errorf("Max-Age = %d, want the session's lifetime", cookie.MaxAge)
	}
}

// A dokkup behind plain HTTP must not set Secure: the browser would drop the
// cookie and the operator would meet a sign-in screen that reports success and
// then asks them to sign in again, with nothing in any log to say why.
func TestOnPlainHTTPTheSessionCookieIsNotSecure(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModeIP, PlainHTTP: true})
	makeOwner(t, st, goodEmail, goodPassword)

	if cookie := signIn(t, srv, goodEmail, goodPassword); cookie.Secure {
		t.Error("the session cookie is Secure on a host reached over plain HTTP")
	}
}

// Signing out has to revoke server-side. A cookie the client throws away is
// not a sign-out: anybody who copied the token out of the browser first would
// go on being signed in.
func TestSigningOutRevokesTheSessionAndAReplayFails(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModePublished})
	makeOwner(t, st, goodEmail, goodPassword)

	cookie := signIn(t, srv, goodEmail, goodPassword)

	resp, raw := do(t, srv, http.MethodPost, "/api/signout", "", true, cookie)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusNoContent, raw)
	}
	if len(raw) != 0 {
		t.Errorf("body = %q, want none", raw)
	}

	cleared := sessionCookieOf(t, resp)
	if cleared.Value != "" || cleared.MaxAge > 0 {
		t.Errorf("the cookie was not cleared: value = %q, Max-Age = %d", cleared.Value, cleared.MaxAge)
	}

	if _, err := st.Session(t.Context(), secret.HashToken(cookie.Value)); err == nil {
		t.Error("the session row survived the sign-out")
	}

	// The replay: the same cookie the browser held a moment ago, sent by
	// somebody who kept a copy.
	resp, raw = do(t, srv, http.MethodGet, probePath, "", false, cookie)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a replayed cookie status = %d, want %d: %s", resp.StatusCode, http.StatusUnauthorized, raw)
	}
	if got := errorOf(t, raw); got != "you are not signed in" {
		t.Errorf("error = %q, want the not-signed-in refusal", got)
	}
}

// The property, not the list: a route nothing allowlisted is refused, and it is
// refused because it was never allowlisted rather than because somebody
// remembered to guard it.
func TestARouteNothingAllowlistedNeedsASession(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModePublished})
	makeOwner(t, st, goodEmail, goodPassword)

	resp, raw := do(t, srv, http.MethodGet, probePath, "", false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusUnauthorized, raw)
	}
	if got := errorOf(t, raw); got != "you are not signed in" {
		t.Errorf("error = %q, want the not-signed-in refusal", got)
	}

	// A cookie naming a session that was never issued is the same refusal: a
	// caller must not be able to tell a forged token from an expired one.
	forged := &http.Cookie{Name: sessionCookieName, Value: unknownToken(t)}
	if resp, _ := do(t, srv, http.MethodGet, probePath, "", false, forged); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a forged cookie status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// The four routes that must stay open, because each of them is something a
// caller needs before they can possibly have a session.
func TestTheAllowlistedRoutesAnswerWithoutASession(t *testing.T) {
	t.Parallel()

	srv, _, _ := newAuthTest(t, Config{Mode: ModePublished})

	for path, method := range map[string]string{
		"/api/health":  http.MethodGet,
		"/api/session": http.MethodGet,
		"/api/setup":   http.MethodPost,
		"/api/signin":  http.MethodPost,
	} {
		resp, raw := do(t, srv, method, path, "{}", true)
		// Asserted on the refusal rather than on the status, because an empty
		// body is a bad request to two of these and a refused sign-in to a
		// third -- all of which mean the route was reached, which is the whole
		// question here.
		if resp.StatusCode == http.StatusUnauthorized && errorOf(t, raw) == "you are not signed in" {
			t.Errorf("%s %s is refused to a caller who cannot yet have a session: %s", method, path, raw)
		}
	}
}

// Nothing outside /api is touched by either guard: the frontend has to load
// before anybody can sign in to it.
func TestTheApplicationShellIsServedWithoutASession(t *testing.T) {
	t.Parallel()

	srv, _, _ := newAuthTest(t, Config{Mode: ModePublished})

	if resp, _ := do(t, srv, http.MethodGet, "/some/route/the/frontend/owns", "", false); resp.StatusCode == http.StatusUnauthorized {
		t.Error("the application shell is behind the session guard")
	}
}

// The CSRF header is the second factor beside SameSite, and a session is
// exactly the state in which a forged cross-site request would be worth making.
func TestAMutationWithASessionButNoCSRFHeaderIsRejected(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModePublished})
	makeOwner(t, st, goodEmail, goodPassword)

	cookie := signIn(t, srv, goodEmail, goodPassword)

	resp, raw := do(t, srv, http.MethodPost, probePath, "{}", false, cookie)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusForbidden, raw)
	}

	// And the same request with the header goes through, so that the assertion
	// above is about the header rather than about the route being broken.
	if resp, raw := do(t, srv, http.MethodPost, probePath, "{}", true, cookie); resp.StatusCode != http.StatusOK {
		t.Errorf("status with the header = %d, want %d: %s", resp.StatusCode, http.StatusOK, raw)
	}
}

// Signing out is a mutation too, and one an attacker would enjoy forging.
func TestSigningOutNeedsTheCSRFHeader(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModePublished})
	makeOwner(t, st, goodEmail, goodPassword)

	cookie := signIn(t, srv, goodEmail, goodPassword)

	if resp, raw := do(t, srv, http.MethodPost, "/api/signout", "", false, cookie); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusForbidden, raw)
	}
	if _, err := st.Session(t.Context(), secret.HashToken(cookie.Value)); err != nil {
		t.Error("a sign-out without the CSRF header ended the session anyway")
	}
}

// Every refusal of the credentials is one string and one status. A caller who
// could tell them apart could enumerate the operators of this host.
func TestEveryRefusedSignInIsTheSameAnswer(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModePublished})
	makeOwner(t, st, goodEmail, goodPassword)

	cases := map[string]string{
		"unknown operator": signinJSON(t, "nobody@example.com", goodPassword),
		"wrong password":   signinJSON(t, goodEmail, "not-the-password"),
		"empty password":   signinJSON(t, goodEmail, ""),
		"password too long": signinJSON(t, goodEmail,
			strings.Repeat("x", maxPasswordBytes+1)),
	}

	for name, body := range cases {
		resp, raw := do(t, srv, http.MethodPost, "/api/signin", body, true)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want %d: %s", name, resp.StatusCode, http.StatusUnauthorized, raw)
		}
		// Compared against the literal Contract froze rather than against the
		// constant the handler answers with: a test that compares the handler
		// to itself stays green through a rename that broke every client.
		if got := errorOf(t, raw); got != "that email address and password do not match" {
			t.Errorf("%s: error = %q, want the one refusal", name, got)
		}
		if len(resp.Cookies()) != 0 {
			t.Errorf("%s: a refused sign-in set a cookie", name)
		}
	}
}

// The timing equalisation, asserted at the thing that makes it work rather than
// with a stopwatch: the dummy is a real argon2id hash at today's cost, so
// verifying against it does the same work verifying a real operator's password
// does. A stopwatch here would measure the machine the test runs on.
func TestTheDummyHashCostsWhatARealVerificationCosts(t *testing.T) {
	t.Parallel()

	hash := dummyPasswordHash()
	if hash == "" {
		t.Fatal("there is no dummy hash, so an unknown operator is refused without paying for argon2")
	}

	real, err := secret.HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("hashing a password: %v", err)
	}
	// The cost is written into the PHC string, so comparing the parameter
	// fields is comparing the work. Raising the cost in internal/secret moves
	// both, which is exactly the property being pinned.
	if got, want := costFields(t, hash), costFields(t, real); got != want {
		t.Errorf("the dummy hash costs %q, a real one costs %q", got, want)
	}

	matches, err := secret.VerifyPassword(hash, goodPassword)
	if err != nil {
		t.Fatalf("verifying against the dummy hash: %v", err)
	}
	if matches {
		t.Error("a password verifies against the dummy hash")
	}
}

// The dummy hash only equalises the timing if the unknown-operator path
// actually verifies against it. That call has no effect on the response, so
// nothing about the answer can show whether it happened -- delete it and every
// other test here stays green while the clock goes back to telling an attacker
// which addresses are operators on this host. The verifier is a field so that
// this test can watch it.
func TestAnUnknownOperatorIsStillVerifiedAgainstTheDummyHash(t *testing.T) {
	t.Parallel()

	s, st, _ := newAuthServer(t, Config{Mode: ModePublished})
	makeOwner(t, st, goodEmail, goodPassword)

	// Guarded because the handler runs on the server's goroutine.
	var (
		mu       sync.Mutex
		verified []string
	)
	real := s.verifyPassword
	s.verifyPassword = func(encoded, password string) (bool, error) {
		mu.Lock()
		verified = append(verified, encoded)
		mu.Unlock()
		// The real function, so that the cost is really paid and the test is
		// not asserting against a stub that returns instantly.
		return real(encoded, password)
	}

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, raw := do(t, srv, http.MethodPost, "/api/signin",
		signinJSON(t, "nobody@example.com", goodPassword), true)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusUnauthorized, raw)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(verified) != 1 {
		t.Fatalf("an unknown operator was verified %d times, want once: the argon2 cost a real "+
			"operator pays is not being paid, and the difference is a clock", len(verified))
	}
	if verified[0] != dummyPasswordHash() {
		t.Errorf("the unknown operator was verified against something other than the dummy hash")
	}
}

// costFields pulls the "$argon2id$v=19$m=...,t=...,p=..." prefix out of a PHC
// string: everything that decides what a verification costs, and nothing that
// differs between two hashes of different passwords.
func costFields(t *testing.T, encoded string) string {
	t.Helper()

	fields := strings.Split(encoded, "$")
	if len(fields) != 6 {
		t.Fatalf("%q is not a PHC string", encoded)
	}
	return strings.Join(fields[1:4], "$")
}

// IP Mode restricts dokkup to the Owner (ADR-0006), and the restriction is
// enforced here or nowhere: the frontend's banner is an affordance.
func TestInIPModeANonOwnerCannotSignIn(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModeIP})
	makeOwner(t, st, goodEmail, goodPassword)
	invited := makeOperator(t, st, "grace@example.com", goodPassword, false)

	resp, raw := do(t, srv, http.MethodPost, "/api/signin",
		signinJSON(t, invited.Email, goodPassword), true)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusUnauthorized, raw)
	}
	// Indistinguishable from a wrong password on purpose: a distinct answer
	// would confirm that the password just typed was the right one.
	if got := errorOf(t, raw); got != "that email address and password do not match" {
		t.Errorf("error = %q, want the one refusal", got)
	}
	if len(resp.Cookies()) != 0 {
		t.Error("a refused sign-in set a cookie")
	}

	// The Owner still signs in, so the assertion above is about who they are
	// rather than about IP Mode refusing everybody.
	if cookie := signIn(t, srv, goodEmail, goodPassword); cookie.Value == "" {
		t.Error("the owner cannot sign in in IP mode")
	}

	// The refusal is recorded against the operator it refused, because this is
	// the one refusal that verified the credentials and therefore knows.
	var found bool
	for _, entry := range auditEntries(t, st) {
		if entry.Action == auditSessionRejected && entry.Target == invited.Email {
			found = true
			if entry.OperatorID != invited.ID {
				t.Errorf("%s is attributed to operator %d, want %d",
					entry.Action, entry.OperatorID, invited.ID)
			}
		}
	}
	if !found {
		t.Errorf("no %s entry for the operator IP mode refused", auditSessionRejected)
	}
}

func TestSessionReportsWhoIsSignedIn(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModeIP})
	makeOwner(t, st, goodEmail, goodPassword)

	// Before signing in: no operator at all, and the field is absent rather
	// than null, so that a client has one thing to test instead of two.
	before := sessionBody(t, srv)
	if before["authenticated"] != false {
		t.Errorf("authenticated = %v, want false", before["authenticated"])
	}
	if _, ok := before["operator"]; ok {
		t.Errorf("operator = %v on a session nobody holds", before["operator"])
	}
	if before["ownerOnly"] != true {
		t.Errorf("ownerOnly = %v, want true in IP mode", before["ownerOnly"])
	}
	if before["setupCompleted"] != true {
		t.Errorf("setupCompleted = %v, want true: this installation has an owner", before["setupCompleted"])
	}

	cookie := signIn(t, srv, goodEmail, goodPassword)

	after := sessionBody(t, srv, cookie)
	if after["authenticated"] != true {
		t.Errorf("authenticated = %v, want true", after["authenticated"])
	}
	operator, ok := after["operator"].(map[string]any)
	if !ok {
		t.Fatalf("operator = %v, want the signed-in operator", after["operator"])
	}
	if operator["email"] != goodEmail || operator["isOwner"] != true {
		t.Errorf("operator = %v, want %s and the owner", operator, goodEmail)
	}
	// The hash is the one field that must never travel, whatever else this
	// object grows.
	if _, leaked := operator["passwordHash"]; leaked {
		t.Error("the session reports the operator's password hash")
	}
}

// The trail is what an operator reads to find out what happened to their host,
// and it is kept forever -- so it has to hold the sign-ins and it has to hold
// nothing that is a credential.
func TestTheTrailRecordsSignInsSignOutsAndFailures(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModePublished})
	owner := makeOwner(t, st, goodEmail, goodPassword)

	if resp, _ := do(t, srv, http.MethodPost, "/api/signin",
		signinJSON(t, goodEmail, "not-the-password"), true); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong password was not refused: %d", resp.StatusCode)
	}

	cookie := signIn(t, srv, goodEmail, goodPassword)

	if resp, _ := do(t, srv, http.MethodPost, "/api/signout", "", true, cookie); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the sign-out failed: %d", resp.StatusCode)
	}

	seen := map[string]store.AuditEntry{}
	for _, entry := range auditEntries(t, st) {
		seen[entry.Action] = entry

		// Nothing a caller typed as a secret, and nothing this server minted as
		// one, may be anywhere in an entry.
		for field, value := range map[string]string{
			"operator email": entry.OperatorEmail,
			"target":         entry.Target,
			"config key":     entry.ConfigKey,
			"action":         entry.Action,
		} {
			for secretName, leaked := range map[string]string{
				"the password":      goodPassword,
				"the session token": cookie.Value,
				"the token's hash":  secret.HashToken(cookie.Value),
			} {
				if strings.Contains(value, leaked) {
					t.Errorf("the %s of a %s entry carries %s", field, entry.Action, secretName)
				}
			}
		}
	}

	started, ok := seen[auditSessionStarted]
	if !ok {
		t.Fatalf("no %s entry in the trail", auditSessionStarted)
	}
	if started.OperatorID != owner.ID || started.Target != owner.Email {
		t.Errorf("%s = operator %d target %q, want %d and %q",
			auditSessionStarted, started.OperatorID, started.Target, owner.ID, owner.Email)
	}

	ended, ok := seen[auditSessionEnded]
	if !ok {
		t.Fatalf("no %s entry in the trail", auditSessionEnded)
	}
	if ended.OperatorID != owner.ID || ended.Target != owner.Email {
		t.Errorf("%s = operator %d target %q, want %d and %q",
			auditSessionEnded, ended.OperatorID, ended.Target, owner.ID, owner.Email)
	}

	rejected, ok := seen[auditSessionRejected]
	if !ok {
		t.Fatalf("no %s entry in the trail", auditSessionRejected)
	}
	// A wrong password proves nothing about who typed it, so the address is
	// recorded as the claim it is and the entry names no operator.
	if rejected.Target != goodEmail {
		t.Errorf("%s target = %q, want the address the attempt claimed", auditSessionRejected, rejected.Target)
	}
	if rejected.OperatorID != 0 {
		t.Errorf("%s is attributed to operator %d: a wrong password is not proof of who typed it",
			auditSessionRejected, rejected.OperatorID)
	}
}

// The limit is what stands between a stranger and unlimited guesses at the
// Owner's password. It is asserted through a clock the test owns, because the
// alternative is waiting a minute.
func TestSignInIsRateLimited(t *testing.T) {
	t.Parallel()

	srv, st, clock := newAuthTest(t, Config{Mode: ModePublished})
	makeOwner(t, st, goodEmail, goodPassword)

	wrong := signinJSON(t, goodEmail, "not-the-password")
	for attempt := range signinBurst {
		if resp, raw := do(t, srv, http.MethodPost, "/api/signin", wrong, true); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want %d: %s", attempt+1, resp.StatusCode, http.StatusUnauthorized, raw)
		}
	}

	resp, raw := do(t, srv, http.MethodPost, "/api/signin", wrong, true)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusTooManyRequests, raw)
	}
	if got := errorOf(t, raw); got != "too many attempts" {
		t.Errorf("error = %q, want the flood refusal", got)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After, so a client retries immediately")
	}

	// The refusal is not audited: it is unbounded, so recording it would let
	// anybody grow the trail as fast as they can open sockets.
	if entries := auditEntries(t, st); len(entries) != signinBurst {
		t.Errorf("the trail has %d entries after %d attempts and one flood refusal, want %d",
			len(entries), signinBurst+1, signinBurst)
	}

	// And the budget comes back, so a mistyped password does not lock an
	// operator out for longer than the window.
	clock.advance(signinRefill)
	if resp, raw := do(t, srv, http.MethodPost, "/api/signin", wrong, true); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after a refill: status = %d, want %d: %s", resp.StatusCode, http.StatusUnauthorized, raw)
	}
}

// The two buckets are separate so that a stranger hammering the setup route --
// which costs them nothing once the Owner exists -- cannot spend the budget the
// Owner needs to sign in with.
func TestFloodingSetupDoesNotSpendTheSignInBudget(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModePublished})
	makeOwner(t, st, goodEmail, goodPassword)

	// The password is deliberately too short, so each attempt is refused before
	// argon2 is paid for. What drains is the setup bucket, which is taken
	// first of all -- and the bucket is the only thing this test is about.
	for range setupBurst + 2 {
		_, _ = do(t, srv, http.MethodPost, "/api/setup",
			setupJSON(t, unknownToken(t), "mallory@example.com", "Mallory", "short"), true)
	}

	if cookie := signIn(t, srv, goodEmail, goodPassword); cookie.Value == "" {
		t.Error("the owner cannot sign in after somebody flooded the setup route")
	}
}

// A dokkup started without a database can still say what it is; it just cannot
// authenticate anybody. Saying so with a 503 is what tells an operator that the
// installation is misconfigured rather than broken.
func TestWithNoDatabaseSignInIsUnavailableAndTheSessionStillAnswers(t *testing.T) {
	t.Parallel()

	s := New(Config{Mode: ModeIP, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, raw := do(t, srv, http.MethodPost, "/api/signin", signinJSON(t, goodEmail, goodPassword), true)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusServiceUnavailable, raw)
	}
	if got := errorOf(t, raw); got != "this dokkup has no database" {
		t.Errorf("error = %q, want the no-database answer", got)
	}

	if body := sessionBody(t, srv); body["authenticated"] != false {
		t.Errorf("authenticated = %v, want false", body["authenticated"])
	}
}

// The bare paths exist so that a wrong method meets an answer rather than the
// application shell with a 200, which is what a client with a bug reads as
// success.
func TestAWrongMethodOnASessionRouteIsNotTheApplicationShell(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModePublished})
	makeOwner(t, st, goodEmail, goodPassword)

	resp, raw := do(t, srv, http.MethodGet, "/api/signin", "", false)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/signin status = %d, want %d: %s", resp.StatusCode, http.StatusMethodNotAllowed, raw)
	}
	if allow := resp.Header.Get("Allow"); allow != http.MethodPost {
		t.Errorf("GET /api/signin Allow = %q, want POST", allow)
	}

	// /api/signout is not in the allowlist, so a stranger meets the session
	// guard before the method check and is told the one thing that is true of
	// them. The bare path is still registered, so a signed-in operator whose
	// client used the wrong method meets the 405 rather than the shell -- and
	// the guard running first is the order that keeps the route table from
	// answering questions to people who have not signed in.
	resp, raw = do(t, srv, http.MethodGet, "/api/signout", "", false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api/signout status = %d, want %d: %s", resp.StatusCode, http.StatusUnauthorized, raw)
	}

	cookie := signIn(t, srv, goodEmail, goodPassword)
	resp, raw = do(t, srv, http.MethodGet, "/api/signout", "", false, cookie)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/signout with a session = %d, want %d: %s",
			resp.StatusCode, http.StatusMethodNotAllowed, raw)
	}
}

func TestABadSignInBodyIsRefusedWithoutQuotingIt(t *testing.T) {
	t.Parallel()

	srv, st, _ := newAuthTest(t, Config{Mode: ModePublished})
	makeOwner(t, st, goodEmail, goodPassword)

	resp, raw := do(t, srv, http.MethodPost, "/api/signin", `{"password": "`+goodPassword, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusBadRequest, raw)
	}
	if got := errorOf(t, raw); got != "that request body is not valid json" {
		t.Errorf("error = %q, want the malformed-body answer", got)
	}
	// The decoder's error quotes what it choked on, and what it choked on was a
	// password.
	if strings.Contains(string(raw), goodPassword) {
		t.Error("the refusal echoes the password back")
	}
}
