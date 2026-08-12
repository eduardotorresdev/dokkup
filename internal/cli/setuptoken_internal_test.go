package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/secret"
	"github.com/eduardotorresdev/dokkup/internal/store"
)

// setupTokenOnScreen matches the one line a token is printed on: indented, on
// its own, and forty-three characters of the base64url alphabet, which is what
// [secret.NewToken]'s thirty-two random bytes come to.
//
// Reading the token back off the output rather than out of the store is the
// point of these tests. The operator has nothing but what is on their screen,
// so a token the database would accept and the terminal never showed is a
// failure this suite has to be able to see.
var setupTokenOnScreen = regexp.MustCompile(`(?m)^\s+([A-Za-z0-9_-]{43})$`)

// setupTokenCommand runs `dokkup` the way a shell does, so that the exit code
// is part of what is asserted: an unattended caller reads it, and "refused"
// against "issued" is the whole of the contract for the second test below.
func setupTokenCommand(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = Run(Env{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errOut,
		Build:  Build{Version: "v0.0.0-test"},
	}, args)
	return out.String(), errOut.String(), code
}

// setupTokenDatabase is a path in a directory of this test's own, which is what
// --db exists for: nothing here goes near [hostpaths.DB], and none of it needs
// root.
func setupTokenDatabase(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "dokkup.db")
}

func setupTokenStore(t *testing.T, path string) *store.Store {
	t.Helper()

	s, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("opening the database at %s: %v", path, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing the database at %s: %v", path, err)
		}
	})
	return s
}

// setupTokensHeld counts the live rows directly, in SQL, because there is no
// call that answers "how many tokens are there" -- deliberately, since nothing
// in dokkup needs to know. A refusal that quietly left a token behind would
// otherwise be invisible until somebody spent it.
func setupTokensHeld(t *testing.T, path string) int {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("opening %s to count setup tokens: %v", path, err)
	}
	defer func() { _ = db.Close() }()

	var held int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM setup_tokens`).Scan(&held); err != nil {
		t.Fatalf("counting the setup tokens in %s: %v", path, err)
	}
	return held
}

func setupTokenFromOutput(t *testing.T, stdout string) string {
	t.Helper()

	found := setupTokenOnScreen.FindAllStringSubmatch(stdout, -1)
	if len(found) != 1 {
		t.Fatalf("the output shows %d tokens, want exactly one:\n%s", len(found), stdout)
	}
	return found[0][1]
}

func TestSetupTokenIssuesATokenThatCreatesTheOwner(t *testing.T) {
	t.Parallel()

	path := setupTokenDatabase(t)

	stdout, stderr, code := setupTokenCommand(t, "setup-token", "--db", path)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr)
	}

	token := setupTokenFromOutput(t, stdout)

	// The token on screen is the token in the database, established the only
	// way that means anything: by spending it. If the printed one were a
	// different draw, or the hash stored were of something else, this is the
	// call that would refuse -- which is exactly how the operator would find
	// out, on a host they had just installed.
	s := setupTokenStore(t, path)
	owner, err := s.RedeemSetupToken(t.Context(), secret.HashToken(token),
		"ops@example.com", "Ada", "an argon2id string")
	if err != nil {
		t.Fatalf("redeeming the token that was printed: %v", err)
	}
	if !owner.IsOwner {
		t.Error("redeeming the printed token created an operator who is not the owner")
	}

	// Printing a secret into scrollback is only defensible if the operator is
	// told why, so the sentence that says so is part of the command rather than
	// something the documentation carries separately.
	for _, said := range []string{"once", "stops working at", "scrollback"} {
		if !strings.Contains(stdout, said) {
			t.Errorf("the output never says %q, so nothing tells the operator why a secret "+
				"on screen is acceptable:\n%s", said, stdout)
		}
	}
}

func TestSetupTokenRefusesOnceThereIsAnOwnerAndIssuesNothing(t *testing.T) {
	t.Parallel()

	path := setupTokenDatabase(t)
	s := setupTokenStore(t, path)
	if _, err := s.CreateOwner(t.Context(), "ops@example.com", "Ada", "an argon2id string"); err != nil {
		t.Fatalf("creating the owner this installation already has: %v", err)
	}

	stdout, stderr, code := setupTokenCommand(t, "setup-token", "--db", path)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1; issuing was refused, and an unattended caller "+
			"reads that off the exit code", code)
	}
	if !strings.Contains(stderr, "already has an owner") {
		t.Errorf("the refusal does not say why:\n%s", stderr)
	}
	if found := setupTokenOnScreen.FindString(stdout); found != "" {
		t.Errorf("a token was printed by a run that refused to issue one: %q", found)
	}
	// The refusal is worth nothing if it left a credential behind: a token in
	// the database with an owner already created is a redemption waiting for
	// whoever finds it.
	if held := setupTokensHeld(t, path); held != 0 {
		t.Errorf("%d setup tokens are in the database after a refusal, want none", held)
	}
}

func TestSetupTokenLeavesTheTrailAndTheOutputFreeOfEverythingButTheToken(t *testing.T) {
	t.Parallel()

	path := setupTokenDatabase(t)

	stdout, stderr, code := setupTokenCommand(t, "setup-token", "--db", path)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr)
	}
	token := setupTokenFromOutput(t, stdout)
	hash := secret.HashToken(token)

	s := setupTokenStore(t, path)
	entries, err := s.AuditEntries(t.Context(), 0)
	if err != nil {
		t.Fatalf("reading the audit trail: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the trail holds %d entries after one issue, want 1: %+v", len(entries), entries)
	}

	entry := entries[0]
	if entry.Action != setupTokenIssuedAction {
		t.Errorf("the trail records %q, want %q", entry.Action, setupTokenIssuedAction)
	}
	// Nobody was signed in to issue it, and the entry has to say so rather than
	// naming somebody: this is the one act dokkup performs for a stranger.
	if entry.OperatorID != 0 || entry.OperatorEmail != "" {
		t.Errorf("the issue is attributed to the operator %d (%q), and nobody was signed in",
			entry.OperatorID, entry.OperatorEmail)
	}

	// Everywhere the token could have been written down, against everything it
	// must not be: the trail as a reader of the database would see it, the
	// error stream, and the database on disk, which holds the hash and must
	// never hold the token.
	//
	// The sidecars as well as the database, because this is a negative
	// assertion and a negative assertion that reads the wrong file stays green
	// for ever. Anything SQLite has not checkpointed yet is in "-wal", which is
	// exactly where a token written by mistake would be sitting.
	dbBytes := setupTokenBytesOnDisk(t, path)
	for where, content := range map[string]string{
		"the audit trail":          fmt.Sprintf("%+v", entries),
		"the error stream":         stderr,
		"the database and its log": dbBytes,
		"the output beyond that":   strings.Replace(stdout, token, "", 1),
	} {
		if strings.Contains(content, token) {
			t.Errorf("%s carries the token itself", where)
		}
	}
	for where, content := range map[string]string{
		"the audit trail":  fmt.Sprintf("%+v", entries),
		"the output":       stdout,
		"the error stream": stderr,
	} {
		if strings.Contains(content, hash) {
			t.Errorf("%s carries the token's hash, which is what recognises it coming back", where)
		}
	}
}

func TestInstallPrintsTheTokenThatCreatesTheOwner(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	h.inst.cfg.plainHTTP = true

	if err := h.install(); err != nil {
		t.Fatalf("install: %v", err)
	}

	report := h.stdout.String()
	if !strings.Contains(report, installToken) {
		t.Errorf("the installation report does not print the token it issued:\n%s", report)
	}
	// Under the address, because a token is useless without somewhere to spend
	// it and the operator should not have to scroll up for one of the two.
	if !strings.Contains(report, h.proved.url) {
		t.Errorf("the report prints a token but not the address to spend it at:\n%s", report)
	}
}

func TestInstallThatCouldNotIssueATokenIsStillAnInstallation(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	h.inst.cfg.plainHTTP = true
	h.inst.issueToken = func(context.Context) (setupTokenIssued, error) {
		return setupTokenIssued{}, errors.New("the database would not open")
	}

	// Not an error, and nothing unwound: what failed is one command's worth of
	// work on a host that is otherwise serving.
	if err := h.install(); err != nil {
		t.Fatalf("install: %v", err)
	}
	for _, path := range []string{hostpaths.Unit, hostpaths.Binary} {
		if !h.has(path) {
			t.Errorf("%s is gone, so a failed issue unwound an installation that had worked", path)
		}
	}

	report := h.stdout.String()
	for _, said := range []string{
		"installed and answering",
		"the database would not open",
		"dokkup setup-token",
	} {
		if !strings.Contains(report, said) {
			t.Errorf("the report never says %q, so it neither reports the failure nor says "+
				"what to run:\n%s", said, report)
		}
	}
}

// setupTokenBytesOnDisk is everything this database occupies: the file and the
// two SQLite may have left beside it. A missing sidecar is the ordinary case
// after a store that closed cleanly, and reading them anyway is what keeps a
// negative assertion honest if one day it does not close cleanly.
func setupTokenBytesOnDisk(t *testing.T, path string) string {
	t.Helper()

	var all strings.Builder
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		content, err := os.ReadFile(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("reading %s back: %v", file, err)
		}
		all.Write(content)
	}
	return all.String()
}

// setupTokenFakeHost is a host whose euid, service account and chown are this
// test's to choose, which is the only way the root path is reachable at all:
// the suite does not run as root, and the failures that matter there lock the
// service out of its own database.
type setupTokenFakeHost struct {
	t *testing.T

	euid      int
	uid, gid  int
	lookupErr error

	// refuse is what chown answers for a path, standing in for the EPERM a
	// hardened mount or a immutable flag would produce.
	refuse map[string]error

	// given is what was actually handed over, in order.
	given []string
}

func (h *setupTokenFakeHost) host() setupTokenHost {
	return setupTokenHost{
		geteuid: func() int { return h.euid },
		account: func() (int, int, error) {
			if h.euid != 0 {
				h.t.Error("the service account was looked up by a process that is not root")
			}
			if h.lookupErr != nil {
				return 0, 0, h.lookupErr
			}
			return h.uid, h.gid, nil
		},
		chown: func(path string, uid, gid int) error {
			if uid != h.uid || gid != h.gid {
				h.t.Errorf("%s was given to %d:%d, want the service account %d:%d",
					path, uid, gid, h.uid, h.gid)
			}
			if err := h.refuse[path]; err != nil {
				return err
			}
			// The real one answers ErrNotExist for a sidecar that is not there,
			// and the caller is required to treat that as the ordinary case
			// rather than as a host to warn about.
			if _, err := os.Stat(path); err != nil {
				return err
			}
			h.given = append(h.given, path)
			return nil
		},
	}
}

// setupTokenIssueOn runs the whole issue against a host of the test's choosing,
// which is what runSetupToken does with the real one.
func setupTokenIssueOn(t *testing.T, on setupTokenHost, path string) (setupTokenIssued, string, error) {
	t.Helper()

	var errOut bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: &errOut}
	issued, err := setupTokenIssue(t.Context(), env, on, path, TokenLifetime)
	return issued, errOut.String(), err
}

func TestIssuingAsRootHandsTheDatabaseBackToTheService(t *testing.T) {
	t.Parallel()

	path := setupTokenDatabase(t)
	fake := &setupTokenFakeHost{t: t, euid: 0, uid: 990, gid: 991}

	issued, stderr, err := setupTokenIssueOn(t, fake.host(), path)
	if err != nil {
		t.Fatalf("issuing as root: %v", err)
	}
	if issued.token == "" {
		t.Error("issuing as root produced no token")
	}

	// The directory as well as the file. store.Open creates it 0700 and root
	// creating it is what leaves the service without even the traversal bit,
	// which is the same lockout one level up.
	want := []string{filepath.Dir(path), path}
	if !slices.Equal(fake.given, want) {
		t.Errorf("root handed over %v, want %v", fake.given, want)
	}
	// The sidecars are gone by now, because the store was closed first, and a
	// host where that is true is not a host to warn anybody about.
	if stderr != "" {
		t.Errorf("a handover that worked said something: %q", stderr)
	}
}

func TestIssuingBeforeThereIsAServiceAccountStillPrintsTheToken(t *testing.T) {
	t.Parallel()

	path := setupTokenDatabase(t)
	fake := &setupTokenFakeHost{
		t: t, euid: 0, uid: 990, gid: 991,
		lookupErr: user.UnknownUserError(hostpaths.User),
	}

	issued, stderr, err := setupTokenIssueOn(t, fake.host(), path)

	// Not fatal, because the token is already minted: a run that failed here
	// would look to a script exactly like a run that issued nothing.
	if err != nil {
		t.Fatalf("issuing on a host with no %s account: %v", hostpaths.User, err)
	}
	if issued.token == "" {
		t.Error("a missing service account swallowed the token")
	}
	if len(fake.given) != 0 {
		t.Errorf("something was handed to an account this host does not have: %v", fake.given)
	}
	for _, said := range []string{hostpaths.User, "owned by root", "dokkup install"} {
		if !strings.Contains(stderr, said) {
			t.Errorf("the note never says %q, so nothing says what happened:\n%s", said, stderr)
		}
	}
}

func TestAHandoverThatIsRefusedSaysWhatToTypeAndKeepsTheToken(t *testing.T) {
	t.Parallel()

	path := setupTokenDatabase(t)
	fake := &setupTokenFakeHost{
		t: t, euid: 0, uid: 990, gid: 991,
		refuse: map[string]error{path: errors.New("operation not permitted")},
	}

	issued, stderr, err := setupTokenIssueOn(t, fake.host(), path)
	if err != nil {
		t.Fatalf("issuing where the chown is refused: %v", err)
	}
	if issued.token == "" {
		t.Error("a refused chown swallowed the token")
	}

	// The one thing an operator can act on is the command, so it has to be in
	// the message ready to paste rather than described.
	want := fmt.Sprintf("chown %s:%s %s", hostpaths.User, hostpaths.Group, path)
	if !strings.Contains(stderr, want) {
		t.Errorf("the warning does not carry %q, so there is nothing to run:\n%s", want, stderr)
	}
	if !strings.Contains(stderr, "operation not permitted") {
		t.Errorf("the warning does not say why the handover failed:\n%s", stderr)
	}
	// The directory came first and did work, which is the ordering that leaves
	// the recoverable half done.
	if !slices.Contains(fake.given, filepath.Dir(path)) {
		t.Errorf("the directory was never handed over: %v", fake.given)
	}
}

func TestIssuingWithoutRootTouchesNobodysOwnership(t *testing.T) {
	t.Parallel()

	path := setupTokenDatabase(t)
	// Which is every machine this suite runs on, and every developer pointing
	// --db at a copy of a database on their own laptop: nothing was created as
	// root, so there is nothing to give back and nobody to be told about.
	fake := &setupTokenFakeHost{t: t, euid: 1000}

	_, stderr, err := setupTokenIssueOn(t, fake.host(), path)
	if err != nil {
		t.Fatalf("issuing as an ordinary user: %v", err)
	}
	if len(fake.given) != 0 {
		t.Errorf("an unprivileged run changed the ownership of %v", fake.given)
	}
	if stderr != "" {
		t.Errorf("an unprivileged run said something about accounts: %q", stderr)
	}
}

func TestATokenTheTrailCannotRecordIsNeverPrinted(t *testing.T) {
	t.Parallel()

	path := setupTokenDatabase(t)
	setupTokenStore(t, path)

	// A database that has been through the migrations and then lost the table
	// the trail lives in, which is what a hand-edited or half-restored file
	// looks like. There is no seam for this and there should not be one: what
	// is under test is the store refusing, not a fake refusing.
	setupTokenExec(t, path, `DROP TABLE audit_entries`)

	stdout, stderr, code := setupTokenCommand(t, "setup-token", "--db", path)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1: the trail could not record the issue", code)
	}
	if found := setupTokenOnScreen.FindString(stdout); found != "" {
		t.Errorf("a token was printed although nothing recorded that it was issued: %q", found)
	}
	if !strings.Contains(stderr, "audit") {
		t.Errorf("the failure does not say the trail is what went wrong:\n%s", stderr)
	}
	// The token is in the database, and that is the trade this makes: one
	// nobody was shown is spent by nobody and replaced by the next issue,
	// whereas one that was shown and never recorded is a credential to this
	// host the trail says was never created.
	if held := setupTokensHeld(t, path); held != 1 {
		t.Errorf("%d setup tokens are held, want the one that was minted and not printed", held)
	}
}

func setupTokenExec(t *testing.T, path, statement string) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.ExecContext(t.Context(), statement); err != nil {
		t.Fatalf("running %q against %s: %v", statement, path, err)
	}
}

func TestInstallOnAHostThatAlreadyHasItsOwnerReportsNoFailure(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	h.inst.cfg.plainHTTP = true

	// The real issue path against this host's own database, not a stub: an
	// owner that already exists is what every installation after the first one
	// meets, and the report above tells an operator with no --acme-email to
	// re-run install, so this is a path dokkup asks people to take.
	path := filepath.Join(h.root, hostpaths.DB)
	s := setupTokenStore(t, path)
	if _, err := s.CreateOwner(t.Context(), "ops@example.com", "Ada", "an argon2id string"); err != nil {
		t.Fatalf("creating the owner this host already has: %v", err)
	}
	h.inst.issueToken = func(ctx context.Context) (setupTokenIssued, error) {
		fake := &setupTokenFakeHost{t: t, euid: 1000}
		return setupTokenIssue(ctx, h.inst.env, fake.host(), path, TokenLifetime)
	}

	if err := h.install(); err != nil {
		t.Fatalf("reinstalling on a host that has its owner: %v", err)
	}

	report := h.stdout.String()
	if !strings.Contains(report, "already has its owner") {
		t.Errorf("the report does not say why there is no token:\n%s", report)
	}
	// The two things it must not do: describe a working host as broken, and
	// send the operator to a command that refuses with the same sentence and
	// exits 1.
	if strings.Contains(report, "What failed") || strings.Contains(report, "failed is the token") {
		t.Errorf("an installation that worked is reported as a failure:\n%s", report)
	}
	if strings.Contains(report, "Run 'dokkup setup-token'") {
		t.Errorf("the report sends the operator to a command that will refuse:\n%s", report)
	}
	if held := setupTokensHeld(t, path); held != 0 {
		t.Errorf("%d setup tokens were issued to a host that already has its owner, want none", held)
	}
}
