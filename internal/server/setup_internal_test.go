package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/secret"
	"github.com/eduardotorresdev/dokkup/internal/store"
)

// These tests live inside the package rather than beside the ones in
// server_test.go, for one reason: the rate limit is per minute, and asserting a
// per-minute rule from outside would mean either waiting a minute or asserting
// nothing. The clock is a field on the bucket, and reaching it needs the
// package.
const (
	goodPassword = "a-password-nobody-guesses"
	goodEmail    = "ops@example.com"
)

// fakeClock is the injected time. It is mutex-guarded because the bucket reads
// it from whichever goroutine the test server handles the request on, and -race
// is not optional here.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newSetupTest builds a server on a fresh database with a clock the test owns.
func newSetupTest(t *testing.T) (*httptest.Server, *store.Store, *fakeClock) {
	t.Helper()

	st := openTestStore(t)
	srv, clock := newSetupTestWithStore(t, st)
	return srv, st, clock
}

func newSetupTestWithStore(t *testing.T, st *store.Store) (*httptest.Server, *fakeClock) {
	t.Helper()

	clock := &fakeClock{now: time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)}

	// Discarded rather than default, because these tests provoke the error
	// paths on purpose and a passing run must not look like a failing one.
	s := New(Config{Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	s.setupLimiter = newTokenBucket(setupBurst, setupRefill, clock.Now)

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, clock
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "data", "dokkup.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// liveTokenTTL is what a test asks for when it means "a token that is valid",
// and it is an hour rather than a minute for a measured reason: this package
// hashes at 64 MiB a time under -race, several tests do it a dozen times over,
// and a parallel test can genuinely spend more than a minute between being
// issued a token and posting it. That failure looked exactly like the token
// being rejected, which is the answer this route gives to everything.
const liveTokenTTL = time.Hour

// issueToken mints a token and stores its hash, returning the secret the way an
// operator would have it: the plain token, which is the only copy that exists.
func issueToken(t *testing.T, st *store.Store, ttl time.Duration) string {
	t.Helper()

	token, hash, err := secret.NewToken()
	if err != nil {
		t.Fatalf("minting a setup token: %v", err)
	}
	if _, err := st.IssueSetupToken(t.Context(), hash, time.Now().Add(ttl)); err != nil {
		t.Fatalf("issuing a setup token: %v", err)
	}
	return token
}

// unknownToken is a well-formed token that was never issued.
func unknownToken(t *testing.T) string {
	t.Helper()

	token, _, err := secret.NewToken()
	if err != nil {
		t.Fatalf("minting a setup token: %v", err)
	}
	return token
}

func setupJSON(t *testing.T, token, email, name, password string) string {
	t.Helper()

	body, err := json.Marshal(setupRequest{Token: token, Email: email, Name: name, Password: password})
	if err != nil {
		t.Fatalf("encoding a setup request: %v", err)
	}
	return string(body)
}

// send performs one request and reads the whole body, because several of these
// tests compare responses byte for byte and a streamed body cannot be compared
// twice.
func send(t *testing.T, srv *httptest.Server, method, path, body string, csrf bool) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set(csrfHeader, "1")
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

func postSetup(t *testing.T, srv *httptest.Server, body string) (*http.Response, []byte) {
	t.Helper()
	return send(t, srv, http.MethodPost, "/api/setup", body, true)
}

// errorOf reads the one field every failure carries, so that the assertions
// below can be written against the literal strings Contract C froze rather than
// against the constants the handler answers with. A test that compares the
// handler to itself would stay green through a rename that broke every client.
func errorOf(t *testing.T, raw []byte) string {
	t.Helper()

	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}
	return body.Error
}

func auditEntries(t *testing.T, st *store.Store) []store.AuditEntry {
	t.Helper()

	entries, err := st.AuditEntries(t.Context(), 0)
	if err != nil {
		t.Fatalf("reading the audit trail: %v", err)
	}
	return entries
}

func TestRedeemingTheSetupTokenCreatesTheOwner(t *testing.T) {
	t.Parallel()

	srv, st, _ := newSetupTest(t)
	token := issueToken(t, st, liveTokenTTL)

	// Sent with the capitals and the trailing space a browser's autofill would
	// leave, because the address that comes back and the address in the trail
	// must be the one the store would match on later.
	resp, raw := postSetup(t, srv, setupJSON(t, token, " Ops@Example.com ", "Ada", goodPassword))

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusCreated, raw)
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

	owner, err := st.Owner(t.Context())
	if err != nil {
		t.Fatalf("reading the owner back: %v", err)
	}
	if !owner.IsOwner {
		t.Error("the operator that was created is not the owner")
	}

	// The password is checked through the verifier rather than by comparing
	// hashes: what matters is that the operator can sign in with what they
	// typed, not that a particular string was stored.
	ok, err := secret.VerifyPassword(owner.PasswordHash, goodPassword)
	if err != nil {
		t.Fatalf("verifying the stored password: %v", err)
	}
	if !ok {
		t.Error("the stored hash does not verify the password that was sent")
	}

	var redeemed bool
	for _, entry := range auditEntries(t, st) {
		if entry.Action != auditSetupRedeemed {
			continue
		}
		redeemed = true
		if entry.OperatorID != owner.ID {
			t.Errorf("%s is attributed to operator %d, want %d", entry.Action, entry.OperatorID, owner.ID)
		}
		if entry.Target != owner.Email {
			t.Errorf("%s target = %q, want %q", entry.Action, entry.Target, owner.Email)
		}
	}
	if !redeemed {
		t.Errorf("no %s entry in the trail", auditSetupRedeemed)
	}
}

// The token is single-use, and "used" has to mean it even when the second
// attempt is somebody else with a different address.
func TestASecondRedemptionOfTheSameTokenCreatesNothing(t *testing.T) {
	t.Parallel()

	srv, st, _ := newSetupTest(t)
	token := issueToken(t, st, liveTokenTTL)

	if resp, raw := postSetup(t, srv, setupJSON(t, token, goodEmail, "Ada", goodPassword)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first redemption status = %d, want %d: %s", resp.StatusCode, http.StatusCreated, raw)
	}

	resp, raw := postSetup(t, srv, setupJSON(t, token, "mallory@example.com", "Mallory", goodPassword))
	if resp.StatusCode == http.StatusCreated {
		t.Errorf("the token was spent twice: %s", raw)
	}

	operators, err := st.Operators(t.Context())
	if err != nil {
		t.Fatalf("reading the operators: %v", err)
	}
	if len(operators) != 1 {
		t.Fatalf("operators = %d, want 1", len(operators))
	}
	if operators[0].Email != goodEmail {
		t.Errorf("the operator is %s, want %s", operators[0].Email, goodEmail)
	}
}

// The refusal must carry no information about which kind of bad token it was.
// Somebody holding one that has expired must not be able to learn that it was
// ever real, and an attacker guessing must get the same answer every time.
func TestNoResponseTellsBadSetupTokensApart(t *testing.T) {
	t.Parallel()

	srv, st, _ := newSetupTest(t)

	// Expired: issued with a lifetime shorter than the wait below. The store
	// reads the wall clock in the statement that spends the token, so this is
	// the one place a real pause is unavoidable; it is milliseconds.
	expired := issueToken(t, st, 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond)

	// Revoked: reissuing replaces the outstanding token, which is what makes a
	// second `dokkup setup-token` a revocation of the first.
	revoked := issueToken(t, st, liveTokenTTL)
	issueToken(t, st, liveTokenTTL)

	attempt := func(token string) (int, []byte) {
		resp, raw := postSetup(t, srv, setupJSON(t, token, goodEmail, "Ada", goodPassword))
		return resp.StatusCode, raw
	}

	// The reference is taken outside the comparison, so that nothing below ends
	// up being compared with itself.
	referenceStatus, reference := attempt(unknownToken(t))

	if referenceStatus != http.StatusForbidden {
		t.Fatalf("an unknown token answered %d, want %d: %s", referenceStatus, http.StatusForbidden, reference)
	}
	// The literal, not the constant the handler used: Contract C froze this
	// sentence, and a rename that kept the tests green would break every client.
	if got := errorOf(t, reference); got != "that setup token is not valid" {
		t.Errorf("error = %q, want %q", got, "that setup token is not valid")
	}

	for name, token := range map[string]string{"expired": expired, "revoked": revoked} {
		status, raw := attempt(token)
		if status != referenceStatus || !bytes.Equal(raw, reference) {
			t.Errorf("an %s token answered %d %q and an unknown one %d %q: the two are distinguishable",
				name, status, raw, referenceStatus, reference)
		}
	}
}

// Once the host has an owner, every redemption is refused the same way --
// including one carrying the token that created them, which must not be
// distinguishable from a token that never existed.
func TestRedeemingWhenThereIsAlreadyAnOwner(t *testing.T) {
	t.Parallel()

	srv, st, _ := newSetupTest(t)
	token := issueToken(t, st, liveTokenTTL)

	if resp, raw := postSetup(t, srv, setupJSON(t, token, goodEmail, "Ada", goodPassword)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first redemption status = %d, want %d: %s", resp.StatusCode, http.StatusCreated, raw)
	}

	spentResp, spent := postSetup(t, srv, setupJSON(t, token, "mallory@example.com", "Mallory", goodPassword))
	if spentResp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want %d: %s", spentResp.StatusCode, http.StatusConflict, spent)
	}

	if got := errorOf(t, spent); got != "this installation already has an owner" {
		t.Errorf("error = %q, want %q", got, "this installation already has an owner")
	}

	unknownResp, unknown := postSetup(t, srv, setupJSON(t, unknownToken(t), "mallory@example.com", "Mallory", goodPassword))
	if unknownResp.StatusCode != spentResp.StatusCode || !bytes.Equal(unknown, spent) {
		t.Errorf("a spent token answered %d %q and an unknown one %d %q: the two are distinguishable",
			spentResp.StatusCode, spent, unknownResp.StatusCode, unknown)
	}
}

func TestSetupRefusesABodyItCannotAccept(t *testing.T) {
	t.Parallel()

	// The lengths are built from the constants rather than written out, so that
	// raising a bound moves the case with it instead of silently making it
	// assert nothing.
	request := func(email, password string) string {
		return `{"token":"a-token","email":"` + email + `","name":"Ada","password":"` + password + `"}`
	}
	cases := map[string]string{
		"not json":           "{",
		"no email":           request("   ", goodPassword),
		"email too long":     request(strings.Repeat("x", maxEmailBytes)+"@example.com", goodPassword),
		"password too short": request(goodEmail, strings.Repeat("x", minPasswordBytes-1)),
		"password too long":  request(goodEmail, strings.Repeat("x", maxPasswordBytes+1)),
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// A server each, so that no case spends another's rate budget.
			srv, st, _ := newSetupTest(t)

			resp, raw := postSetup(t, srv, body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusBadRequest, raw)
			}

			operators, err := st.Operators(t.Context())
			if err != nil {
				t.Fatalf("reading the operators: %v", err)
			}
			if len(operators) != 0 {
				t.Errorf("operators = %d, want none", len(operators))
			}

			// Every refusal is a security event, and a refused attempt is the
			// one this route exists to record.
			entries := auditEntries(t, st)
			if len(entries) != 1 || entries[0].Action != auditSetupRejected {
				t.Errorf("trail = %+v, want one %s entry", entries, auditSetupRejected)
			}
		})
	}
}

// The password ceiling is a denial-of-service defence, so what matters is that
// it is enforced before argon2 is paid for. A password just under it is hashed
// and accepted, which is what proves the boundary is where it says it is.
func TestTheLongestAllowedPasswordIsAccepted(t *testing.T) {
	t.Parallel()

	srv, st, _ := newSetupTest(t)
	token := issueToken(t, st, liveTokenTTL)

	resp, raw := postSetup(t, srv,
		setupJSON(t, token, goodEmail, "Ada", strings.Repeat("x", maxPasswordBytes)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusCreated, raw)
	}
}

func TestSetupNeedsTheCSRFHeader(t *testing.T) {
	t.Parallel()

	srv, st, _ := newSetupTest(t)
	token := issueToken(t, st, liveTokenTTL)

	resp, raw := send(t, srv, http.MethodPost, "/api/setup",
		setupJSON(t, token, goodEmail, "Ada", goodPassword), false)

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusForbidden, raw)
	}

	operators, err := st.Operators(t.Context())
	if err != nil {
		t.Fatalf("reading the operators: %v", err)
	}
	if len(operators) != 0 {
		t.Errorf("operators = %d, want none: the guard let the request through", len(operators))
	}
}

// Without a pattern on the bare path, the catch-all that serves the frontend
// would answer a GET here with the application shell and a 200, which a client
// with a bug would read as success.
func TestSetupAcceptsNothingButPOST(t *testing.T) {
	t.Parallel()

	srv, _, _ := newSetupTest(t)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		resp, raw := send(t, srv, method, "/api/setup", "", true)

		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want %d: %s", method, resp.StatusCode, http.StatusMethodNotAllowed, raw)
		}
		if allow := resp.Header.Get("Allow"); allow != http.MethodPost {
			t.Errorf("%s Allow = %q, want %q", method, allow, http.MethodPost)
		}
	}
}

// The limit counts attempts, not successes: guessing is done with failures, so
// a counter that only moved on the one attempt that worked would protect
// nothing.
func TestSetupIsRateLimited(t *testing.T) {
	t.Parallel()

	srv, st, clock := newSetupTest(t)
	issueToken(t, st, liveTokenTTL)

	attempt := func() (*http.Response, []byte) {
		return postSetup(t, srv, setupJSON(t, unknownToken(t), goodEmail, "Ada", goodPassword))
	}

	for i := range setupBurst {
		if resp, raw := attempt(); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("attempt %d status = %d, want %d: %s", i+1, resp.StatusCode, http.StatusForbidden, raw)
		}
	}

	resp, raw := attempt()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("attempt %d status = %d, want %d: %s", setupBurst+1, resp.StatusCode, http.StatusTooManyRequests, raw)
	}
	if got := errorOf(t, raw); got != "too many attempts" {
		t.Errorf("error = %q, want %q", got, "too many attempts")
	}

	// A refusal with no Retry-After is an invitation to retry immediately.
	seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q, want whole seconds: %v", resp.Header.Get("Retry-After"), err)
	}
	if seconds < 1 {
		t.Errorf("Retry-After = %d, want at least 1", seconds)
	}

	// A rate limit that never lets go is an outage. One token's worth of time
	// buys exactly one more attempt.
	clock.advance(setupRefill)
	if resp, raw := attempt(); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("after the refill status = %d, want %d: %s", resp.StatusCode, http.StatusForbidden, raw)
	}
	if resp, _ := attempt(); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the refill handed back more than one attempt: status = %d", resp.StatusCode)
	}
}

// SECURITY.md promises the token and the password appear in nothing that is
// kept. The trail is kept forever and read by everyone who can read the
// database, and a response body ends up in a browser's devtools and in whatever
// a support ticket pastes.
func TestNeitherTheTokenNorThePasswordIsEverEchoedOrRecorded(t *testing.T) {
	t.Parallel()

	srv, st, _ := newSetupTest(t)
	token := issueToken(t, st, liveTokenTTL)
	hash := secret.HashToken(token)

	var bodies [][]byte

	_, created := postSetup(t, srv, setupJSON(t, token, goodEmail, "Ada", goodPassword))
	bodies = append(bodies, created)

	_, refused := postSetup(t, srv, setupJSON(t, token, goodEmail, "Ada", goodPassword))
	bodies = append(bodies, refused)

	_, invalid := postSetup(t, srv, `{"token":"`+token+`","email":"`+goodEmail+`","password":"short"}`)
	bodies = append(bodies, invalid)

	secrets := map[string]string{"the token": token, "its hash": hash, "the password": goodPassword}

	for _, body := range bodies {
		for what, leak := range secrets {
			if bytes.Contains(body, []byte(leak)) {
				t.Errorf("a response body carries %s: %s", what, body)
			}
		}
	}

	for _, entry := range auditEntries(t, st) {
		written := strings.Join([]string{entry.Action, entry.Target, entry.ConfigKey, entry.OperatorEmail}, " ")
		for what, leak := range secrets {
			if strings.Contains(written, leak) {
				t.Errorf("the audit entry %q carries %s", written, what)
			}
		}
	}
}

func TestSessionReportsWhetherSetupIsDone(t *testing.T) {
	t.Parallel()

	srv, st, _ := newSetupTest(t)

	if body := sessionBody(t, srv); body["setupCompleted"] != false {
		t.Errorf("setupCompleted = %v before there is an owner, want false", body["setupCompleted"])
	}

	token := issueToken(t, st, liveTokenTTL)
	if resp, raw := postSetup(t, srv, setupJSON(t, token, goodEmail, "Ada", goodPassword)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("redemption status = %d, want %d: %s", resp.StatusCode, http.StatusCreated, raw)
	}

	if body := sessionBody(t, srv); body["setupCompleted"] != true {
		t.Errorf("setupCompleted = %v after the owner was created, want true", body["setupCompleted"])
	}
}

// A dokkup with no database can still say who it is and whether Dokku answers,
// which is what an operator diagnosing a broken host needs. What it cannot do
// is create the owner, and it has to say so rather than fail obscurely.
func TestWithoutAStoreSetupIsUnavailableAndSessionSaysNothingIsDone(t *testing.T) {
	t.Parallel()

	srv, _ := newSetupTestWithStore(t, nil)

	resp, raw := postSetup(t, srv, setupJSON(t, "token", goodEmail, "Ada", goodPassword))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusServiceUnavailable, raw)
	}
	if got := errorOf(t, raw); got != "this dokkup has no database" {
		t.Errorf("error = %q, want %q", got, "this dokkup has no database")
	}

	if body := sessionBody(t, srv); body["setupCompleted"] != false {
		t.Errorf("setupCompleted = %v with no database, want false", body["setupCompleted"])
	}
}

// Two browsers on one token is the case the single transaction in the store
// exists for, and the case no other test here reaches: it is the only way to
// arrive at the ErrOwnerExists branch of the redemption, which is where the
// loser of the race is answered.
func TestSimultaneousRedemptionsOfOneTokenCreateOneOwner(t *testing.T) {
	t.Parallel()

	srv, st, _ := newSetupTest(t)
	token := issueToken(t, st, liveTokenTTL)

	// Exactly the burst, so that the rate limit refuses none of the racers and
	// this stays a test of the transaction rather than of the bucket.
	const racers = setupBurst

	type outcome struct {
		status int
		err    error
	}
	outcomes := make(chan outcome, racers)
	start := make(chan struct{})

	// Built here rather than in the goroutines, so that nothing calls t.Fatalf
	// off the test's own goroutine. Distinct addresses, because two racers
	// sharing one would be refused by the unique index on the address rather
	// than by the index on ownership, which is a different collision.
	bodies := make([]string, racers)
	for i := range bodies {
		bodies[i] = setupJSON(t, token, fmt.Sprintf("ops%d@example.com", i), "Ada", goodPassword)
	}

	for i := range racers {
		go func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/setup",
				strings.NewReader(bodies[i]))
			if err != nil {
				outcomes <- outcome{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(csrfHeader, "1")

			// Released together, so the requests overlap in the store rather
			// than queueing behind each other's argon2.
			<-start

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				outcomes <- outcome{err: err}
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			outcomes <- outcome{status: resp.StatusCode}
		}()
	}
	close(start)

	created := 0
	for range racers {
		got := <-outcomes
		if got.err != nil {
			t.Fatalf("posting a redemption: %v", got.err)
		}
		switch got.status {
		case http.StatusCreated:
			created++
		case http.StatusConflict, http.StatusForbidden:
			// The loser either found the token already spent or lost the insert
			// of the owner. Both are refusals; which one is a matter of
			// microseconds.
		default:
			t.Errorf("a racer answered %d, want one of %d, %d or %d",
				got.status, http.StatusCreated, http.StatusConflict, http.StatusForbidden)
		}
	}

	if created != 1 {
		t.Errorf("%d of %d racers were told they created the owner, want exactly 1", created, racers)
	}

	operators, err := st.Operators(t.Context())
	if err != nil {
		t.Fatalf("reading the operators: %v", err)
	}
	if len(operators) != 1 {
		t.Errorf("operators = %d, want 1", len(operators))
	}
}

// The ceiling on the body is enforced by the reader, before the decoder sees
// anything. The field made huge here is the name, because no rule bounds a
// name: without the reader this body would decode cleanly and be redeemed, so
// removing the limit turns this test red rather than leaving it green.
func TestABodyPastTheCeilingIsRefusedBeforeItIsParsed(t *testing.T) {
	t.Parallel()

	srv, st, _ := newSetupTest(t)
	token := issueToken(t, st, liveTokenTTL)

	resp, raw := postSetup(t, srv,
		setupJSON(t, token, goodEmail, strings.Repeat("n", setupBodyLimit), goodPassword))

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, http.StatusBadRequest, raw)
	}

	operators, err := st.Operators(t.Context())
	if err != nil {
		t.Fatalf("reading the operators: %v", err)
	}
	if len(operators) != 0 {
		t.Errorf("operators = %d, want none: the body was read past the ceiling", len(operators))
	}
}

// sessionBody reads /api/session, optionally as somebody holding a session.
// The cookie is variadic because most callers are asking what an anonymous
// visitor is told, which is what this endpoint mostly exists for.
func sessionBody(t *testing.T, srv *httptest.Server, cookies ...*http.Cookie) map[string]any {
	t.Helper()

	resp, raw := do(t, srv, http.MethodGet, "/api/session", "", false, cookies...)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/session status = %d, want %d: %s", resp.StatusCode, http.StatusOK, raw)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decoding the session: %v", err)
	}
	return body
}
