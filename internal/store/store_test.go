package store_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/store"
)

// hash stands in for whatever the caller stores in the password_hash and
// token_hash columns. The store never interprets either, so a recognisable
// string is more useful in a failure message than a real hash would be.
func hash(of string) string { return "hash-of-" + of }

// openStore opens a store in a directory that does not exist yet, which is the
// state a first installation is in.
func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "data", "dokkup.db")
	opened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("opening a store at %s: %v", path, err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})
	return opened, path
}

// owner creates the Owner and fails the test if it could not, since almost
// every case needs one before it can begin.
func owner(t *testing.T, s *store.Store) store.Operator {
	t.Helper()

	created, err := s.CreateOwner(t.Context(), "owner@example.test", "The Owner", hash("owner"))
	if err != nil {
		t.Fatalf("creating the owner: %v", err)
	}
	return created
}

func TestAnEmptyDirectoryIsMigratedIntoAWorkingStore(t *testing.T) {
	t.Parallel()

	s, path := openStore(t)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the database file is not there after opening: %v", err)
	}

	// Every table the migration creates is exercised, because a migration that
	// half-ran would still leave a file behind.
	created := owner(t, s)
	if _, err := s.StartSession(t.Context(), created.ID, hash("token"), time.Now().Add(time.Hour)); err != nil {
		t.Errorf("starting a session in a freshly migrated store: %v", err)
	}
	if err := s.Record(t.Context(), store.Audited{OperatorID: created.ID, Action: "owner.created"}); err != nil {
		t.Errorf("recording an audit entry in a freshly migrated store: %v", err)
	}
}

func TestReopeningAStoreChangesNothingThatIsInIt(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "dokkup.db")

	first, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	created := owner(t, first)
	if err := first.Record(t.Context(), store.Audited{OperatorID: created.ID, Action: "owner.created"}); err != nil {
		t.Fatalf("recording an audit entry: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}

	// Twice more, because "idempotent" has to mean every restart and not only
	// the second one.
	for restart := range 2 {
		reopened, err := store.Open(t.Context(), path)
		if err != nil {
			t.Fatalf("reopening the store for the %d time: %v", restart+2, err)
		}

		found, err := reopened.Owner(t.Context())
		if err != nil {
			t.Fatalf("reading the owner after restart %d: %v", restart+2, err)
		}
		if found.ID != created.ID || found.Email != created.Email {
			t.Errorf("the owner after restart %d is %+v, want %+v", restart+2, found, created)
		}

		entries, err := reopened.AuditEntries(t.Context(), 0)
		if err != nil {
			t.Fatalf("reading the audit trail after restart %d: %v", restart+2, err)
		}
		if len(entries) != 1 {
			t.Errorf("the audit trail holds %d entries after restart %d, want the 1 that was written",
				len(entries), restart+2)
		}

		if err := reopened.Close(); err != nil {
			t.Fatalf("closing the store: %v", err)
		}
	}
}

func TestTheDatabaseIsNotReadableByAnyoneElseOnTheHost(t *testing.T) {
	t.Parallel()

	s, path := openStore(t)

	// Something has to have been written through the log for the "-wal" and
	// "-shm" files to exist at all, and they are the point of this test.
	created := owner(t, s)
	if _, err := s.StartSession(t.Context(), created.ID, hash("token"), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("starting a session: %v", err)
	}

	if info, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("reading the mode of the data directory: %v", err)
	} else if info.Mode().Perm() != fs.FileMode(0o700) {
		t.Errorf("the data directory is %v, want 0700", info.Mode().Perm())
	}

	// The database holds password hashes and session token hashes. A host
	// account that can read it can sign in as any operator.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Errorf("reading the mode of %s: %v", filepath.Base(path+suffix), err)
			continue
		}
		if info.Mode().Perm() != fs.FileMode(0o600) {
			t.Errorf("%s is %v, want 0600", filepath.Base(path+suffix), info.Mode().Perm())
		}
	}
}

func TestADatabaseLeftReadableByAnOlderInstallationIsTightenedOnOpening(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "dokkup.db")

	// What SQLite would have created for a version of this package that let it.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("writing a world-readable database: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("loosening the directory: %v", err)
	}

	s, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	defer func() { _ = s.Close() }()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("reading the mode of the database: %v", err)
	}
	if info.Mode().Perm() != fs.FileMode(0o600) {
		t.Errorf("the database is still %v after opening, want 0600", info.Mode().Perm())
	}
	if info, err = os.Stat(dir); err != nil {
		t.Fatalf("reading the mode of the directory: %v", err)
	} else if info.Mode().Perm() != fs.FileMode(0o700) {
		t.Errorf("the directory is still %v after opening, want 0700", info.Mode().Perm())
	}
}

func TestThereIsOnlyEverOneOwner(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)

	if _, err := s.Owner(t.Context()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("owner of an installation nobody has set up = %v, want ErrNotFound: "+
			"ADR-0007 reissues a setup token on exactly this answer", err)
	}

	first := owner(t, s)

	_, err := s.CreateOwner(t.Context(), "second@example.test", "Somebody Else", hash("second"))
	if !errors.Is(err, store.ErrOwnerExists) {
		t.Errorf("creating a second owner = %v, want ErrOwnerExists", err)
	}

	// An invited operator is not refused; the index covers only owner rows.
	for _, email := range []string{"one@example.test", "two@example.test"} {
		if _, err := s.CreateOperator(t.Context(), email, "Invited", hash(email)); err != nil {
			t.Fatalf("creating the operator %s: %v", email, err)
		}
	}

	found, err := s.Owner(t.Context())
	if err != nil {
		t.Fatalf("reading the owner: %v", err)
	}
	if found.ID != first.ID {
		t.Errorf("the owner is %d, want the %d that was created first", found.ID, first.ID)
	}
	if !found.IsOwner {
		t.Error("the owner does not report itself as the owner")
	}

	operators, err := s.Operators(t.Context())
	if err != nil {
		t.Fatalf("listing the operators: %v", err)
	}
	if len(operators) != 3 {
		t.Fatalf("there are %d operators, want the 3 that were created", len(operators))
	}
	if !operators[0].IsOwner {
		t.Error("the owner is not listed first")
	}
	for _, operator := range operators[1:] {
		if operator.IsOwner {
			t.Errorf("the operator %s reports itself as an owner as well", operator.Email)
		}
	}
}

func TestTheOwnerCannotBeRemovedAndAnInvitedOperatorCan(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	created := owner(t, s)

	invited, err := s.CreateOperator(t.Context(), "invited@example.test", "Invited", hash("invited"))
	if err != nil {
		t.Fatalf("creating an operator: %v", err)
	}

	if err := s.RemoveOperator(t.Context(), created.ID); !errors.Is(err, store.ErrOwnerProtected) {
		t.Errorf("removing the owner = %v, want ErrOwnerProtected: "+
			"an installation with no owner is one nobody can administer", err)
	}
	if _, err := s.Owner(t.Context()); err != nil {
		t.Errorf("the owner is gone after a refused removal: %v", err)
	}

	if err := s.RemoveOperator(t.Context(), invited.ID); err != nil {
		t.Errorf("removing an invited operator: %v", err)
	}
	if _, err := s.Operator(t.Context(), invited.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading a removed operator = %v, want ErrNotFound", err)
	}
	if err := s.RemoveOperator(t.Context(), invited.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("removing an operator who is not there = %v, want ErrNotFound", err)
	}
}

func TestAnEmailAddressBelongsToOneOperatorHoweverItIsTyped(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	owner(t, s)

	if _, err := s.CreateOperator(t.Context(), "invited@example.test", "Invited", hash("a")); err != nil {
		t.Fatalf("creating an operator: %v", err)
	}

	for name, typed := range map[string]string{
		"exactly as it was":     "invited@example.test",
		"with a capital":        "Invited@example.test",
		"shouted":               "INVITED@EXAMPLE.TEST",
		"with whitespace round": "  invited@example.test  ",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.CreateOperator(t.Context(), typed, "Impostor", hash("b"))
			if !errors.Is(err, store.ErrExists) {
				t.Errorf("creating an operator as %q = %v, want ErrExists: "+
					"shifting a key must not buy a second account", typed, err)
			}
			found, err := s.OperatorByEmail(t.Context(), typed)
			if err != nil {
				t.Fatalf("reading the operator by %q: %v", typed, err)
			}
			if found.Email != "invited@example.test" {
				t.Errorf("the stored address is %q, want it normalised", found.Email)
			}
		})
	}

	if _, err := s.OperatorByEmail(t.Context(), "nobody@example.test"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading an operator who is not there = %v, want ErrNotFound", err)
	}
}

func TestAnOperatorWhoIsNotThereCannotHaveASession(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)

	// This is the foreign key doing its work. Without PRAGMA foreign_keys the
	// insert succeeds and the session authenticates nobody at all.
	_, err := s.StartSession(t.Context(), 4242, hash("token"), time.Now().Add(time.Hour))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("starting a session for an operator who does not exist = %v, want ErrNotFound: "+
			"foreign_keys is off unless every connection is told otherwise", err)
	}
}

func TestForeignKeysHoldOnEveryConnectionInThePoolAndNotOnlyTheFirst(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)

	// foreign_keys is per-connection, so a store that set it once after opening
	// would pass the single-threaded test above and fail here as soon as
	// database/sql opened a second connection under load.
	const attempts = 16

	var (
		wait   sync.WaitGroup
		mu     sync.Mutex
		passed []error
	)
	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := s.StartSession(context.Background(), 4242, hash("token"), time.Now().Add(time.Hour))
			if errors.Is(err, store.ErrNotFound) {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			passed = append(passed, err)
		}()
	}
	wait.Wait()

	if len(passed) > 0 {
		t.Errorf("%d of %d concurrent inserts were not refused by the foreign key: %v",
			len(passed), attempts, passed)
	}
}

func TestRemovingAnOperatorTakesTheirSessionsWithThem(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	owner(t, s)

	invited, err := s.CreateOperator(t.Context(), "invited@example.test", "Invited", hash("invited"))
	if err != nil {
		t.Fatalf("creating an operator: %v", err)
	}
	for _, token := range []string{"laptop", "phone"} {
		if _, err := s.StartSession(t.Context(), invited.ID, hash(token), time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("starting the %s session: %v", token, err)
		}
	}

	if err := s.RemoveOperator(t.Context(), invited.ID); err != nil {
		t.Fatalf("removing the operator: %v", err)
	}

	// ON DELETE CASCADE, which only happens because foreign_keys is on. An
	// account that is gone must not still be signed in anywhere.
	for _, token := range []string{"laptop", "phone"} {
		if _, err := s.SessionOperator(t.Context(), hash(token)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the %s session still authenticates after the operator was removed: %v", token, err)
		}
	}
}

func TestASessionAuthenticatesUntilItIsEndedOrRunsOut(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	created := owner(t, s)

	session, err := s.StartSession(t.Context(), created.ID, hash("token"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("starting a session: %v", err)
	}
	if session.OperatorID != created.ID {
		t.Errorf("the session belongs to %d, want %d", session.OperatorID, created.ID)
	}

	holder, err := s.SessionOperator(t.Context(), hash("token"))
	if err != nil {
		t.Fatalf("reading the operator holding the session: %v", err)
	}
	if holder.ID != created.ID || holder.Email != created.Email {
		t.Errorf("the session is held by %+v, want the owner %+v", holder, created)
	}

	if _, err := s.SessionOperator(t.Context(), hash("a token nobody was given")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a token that was never issued = %v, want ErrNotFound", err)
	}

	// Signing out is a DELETE, so what is revoked is gone rather than flagged.
	if err := s.RevokeSession(t.Context(), hash("token")); err != nil {
		t.Fatalf("ending the session: %v", err)
	}
	if _, err := s.SessionOperator(t.Context(), hash("token")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an ended session still authenticates: %v", err)
	}
	if _, err := s.Session(t.Context(), hash("token")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an ended session can still be read: %v", err)
	}
	if err := s.RevokeSession(t.Context(), hash("token")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ending a session twice = %v, want ErrNotFound", err)
	}
}

func TestATokenThatIsAlreadyASessionIsNotIssuedTwice(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	created := owner(t, s)

	invited, err := s.CreateOperator(t.Context(), "invited@example.test", "Invited", hash("invited"))
	if err != nil {
		t.Fatalf("creating an operator: %v", err)
	}

	if _, err := s.StartSession(t.Context(), created.ID, hash("token"), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("starting a session: %v", err)
	}

	// token_hash is the sessions table's PRIMARY KEY, and SQLite reports a
	// collision there as SQLITE_CONSTRAINT_PRIMARYKEY (1555) rather than as the
	// SQLITE_CONSTRAINT_UNIQUE (2067) an index gives. A store that matched only
	// 2067 would hand the caller the driver's raw error here.
	for name, operatorID := range map[string]int64{
		"by the operator who holds it": created.ID,
		"by somebody else":             invited.ID,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.StartSession(t.Context(), operatorID, hash("token"), time.Now().Add(time.Hour))
			if !errors.Is(err, store.ErrExists) {
				t.Errorf("starting a second session on one token = %v, want ErrExists", err)
			}
		})
	}

	// And the session that was there is untouched by the refusals.
	holder, err := s.SessionOperator(t.Context(), hash("token"))
	if err != nil {
		t.Fatalf("reading the operator holding the session: %v", err)
	}
	if holder.ID != created.ID {
		t.Errorf("the session is now held by %d, want the %d it was issued to", holder.ID, created.ID)
	}
}

func TestASessionThatHasRunOutAuthenticatesNobody(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	created := owner(t, s)

	// A session dead on arrival is a caller bug rather than an expired session,
	// and is refused outright.
	if _, err := s.StartSession(t.Context(), created.ID, hash("stale"), time.Now().Add(-time.Hour)); err == nil {
		t.Error("a session that had already expired was stored")
	}

	// Long enough that a loaded machine cannot spend the whole of it between
	// storing the session and reading it back. At 40ms it could, and did: the
	// whole suite under -race is enough load, and the failure reads as the
	// store refusing a session that is still valid rather than as the clock
	// having moved on.
	const brief = 2 * time.Second
	if _, err := s.StartSession(t.Context(), created.ID, hash("brief"), time.Now().Add(brief)); err != nil {
		t.Fatalf("starting a brief session: %v", err)
	}
	if _, err := s.SessionOperator(t.Context(), hash("brief")); err != nil {
		t.Fatalf("the brief session does not authenticate while it is still valid: %v", err)
	}

	// Waited for rather than slept through, so that a slow machine does not
	// decide the outcome. The expiry is evaluated by the query itself, which is
	// what makes forgetting to check it impossible for a caller.
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := s.SessionOperator(t.Context(), hash("brief"))
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			t.Fatalf("reading the brief session: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("a session that ran out is still authenticating its holder")
		}
		time.Sleep(time.Millisecond)
	}

	// It is still on disk until something clears it, which is deliberate: the
	// read path does not write.
	dropped, err := s.DropExpiredSessions(t.Context())
	if err != nil {
		t.Fatalf("dropping the expired sessions: %v", err)
	}
	if dropped != 1 {
		t.Errorf("dropped %d expired sessions, want the 1 that ran out", dropped)
	}
	if dropped, err = s.DropExpiredSessions(t.Context()); err != nil || dropped != 0 {
		t.Errorf("dropping the expired sessions again = %d, %v, want 0 and no error", dropped, err)
	}
}

func TestSigningOutEverywhereEndsEverySessionOfThatOperatorAndNobodyElses(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	created := owner(t, s)

	invited, err := s.CreateOperator(t.Context(), "invited@example.test", "Invited", hash("invited"))
	if err != nil {
		t.Fatalf("creating an operator: %v", err)
	}

	for _, token := range []string{"owner-laptop", "owner-phone"} {
		if _, err := s.StartSession(t.Context(), created.ID, hash(token), time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("starting the %s session: %v", token, err)
		}
	}
	if _, err := s.StartSession(t.Context(), invited.ID, hash("invited-laptop"), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("starting the invited operator's session: %v", err)
	}

	ended, err := s.RevokeOperatorSessions(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("ending the owner's sessions: %v", err)
	}
	if ended != 2 {
		t.Errorf("ended %d sessions, want the 2 the owner held", ended)
	}
	if _, err := s.SessionOperator(t.Context(), hash("invited-laptop")); err != nil {
		t.Errorf("another operator's session was ended as well: %v", err)
	}

	// Nothing to end is the state this asks for, not a failure.
	if ended, err = s.RevokeOperatorSessions(t.Context(), created.ID); err != nil || ended != 0 {
		t.Errorf("ending the sessions of an operator who has none = %d, %v, want 0 and no error", ended, err)
	}
}

func TestChangingAPasswordEndsTheSessionsThatPasswordOpened(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	created := owner(t, s)

	if _, err := s.StartSession(t.Context(), created.ID, hash("token"), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("starting a session: %v", err)
	}

	// The two are one act. A password changed because somebody else may know it
	// takes nothing away while the sessions it opened are still alive.
	revoked, err := s.SetPassword(t.Context(), created.ID, hash("a new password"))
	if err != nil {
		t.Fatalf("setting the password: %v", err)
	}
	if revoked != 1 {
		t.Errorf("ended %d sessions, want the 1 that was open", revoked)
	}
	if _, err := s.SessionOperator(t.Context(), hash("token")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a session outlived the password that opened it: %v", err)
	}

	found, err := s.Operator(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("reading the operator back: %v", err)
	}
	if found.PasswordHash != hash("a new password") {
		t.Errorf("the stored hash is %q, want the one that was set", found.PasswordHash)
	}

	if _, err := s.SetPassword(t.Context(), 4242, hash("whatever")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("setting the password of an operator who is not there = %v, want ErrNotFound", err)
	}
}

func TestRenamingAnOperatorLeavesTheAddressTheySignInWithAlone(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	created := owner(t, s)

	if err := s.Rename(t.Context(), created.ID, "The Owner, Renamed"); err != nil {
		t.Fatalf("renaming the owner: %v", err)
	}
	found, err := s.Operator(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("reading the operator back: %v", err)
	}
	if found.Name != "The Owner, Renamed" {
		t.Errorf("the name is %q, want the one that was set", found.Name)
	}
	if found.Email != created.Email {
		t.Errorf("renaming changed the address to %q, want %q", found.Email, created.Email)
	}
	if !found.UpdatedAt.After(created.UpdatedAt) && !found.UpdatedAt.Equal(created.UpdatedAt) {
		t.Errorf("updated_at went backwards: %v is before %v", found.UpdatedAt, created.UpdatedAt)
	}

	if err := s.Rename(t.Context(), 4242, "Nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("renaming an operator who is not there = %v, want ErrNotFound", err)
	}
}

func TestAnAuditEntryNamesTheOperatorTheActionAndTheTarget(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	created := owner(t, s)

	for _, action := range []string{"operator.invited", "app.created", "app.removed"} {
		if err := s.Record(t.Context(), store.Audited{
			OperatorID: created.ID,
			Action:     action,
			Target:     "shopfront",
		}); err != nil {
			t.Fatalf("recording %s: %v", action, err)
		}
	}

	entries, err := s.AuditEntries(t.Context(), 0)
	if err != nil {
		t.Fatalf("reading the audit trail: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("the trail holds %d entries, want the 3 that were written", len(entries))
	}
	if entries[0].Action != "app.removed" {
		t.Errorf("the trail begins with %q, want the newest entry first", entries[0].Action)
	}
	for _, entry := range entries {
		if entry.OperatorID != created.ID {
			t.Errorf("the entry %q names the operator %d, want %d", entry.Action, entry.OperatorID, created.ID)
		}
		if entry.OperatorEmail != created.Email {
			t.Errorf("the entry %q names %q, want %q", entry.Action, entry.OperatorEmail, created.Email)
		}
		if entry.Target != "shopfront" {
			t.Errorf("the entry %q names the target %q, want shopfront", entry.Action, entry.Target)
		}
		if entry.RecordedAt.IsZero() {
			t.Errorf("the entry %q was recorded at no time at all", entry.Action)
		}
	}

	if entries, err = s.AuditEntries(t.Context(), 2); err != nil {
		t.Fatalf("reading a limited audit trail: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("a limit of 2 gave %d entries", len(entries))
	}

	// An entry attributed to nobody would be read as a fact, so there is none.
	if err := s.Record(t.Context(), store.Audited{OperatorID: 4242, Action: "app.created"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("recording an entry for an operator who does not exist = %v, want ErrNotFound", err)
	}
	if err := s.Record(t.Context(), store.Audited{OperatorID: created.ID}); err == nil {
		t.Error("an entry naming no action was recorded")
	}
}

func TestAnAuditEntryRecordsWhichConfigurationKeyChangedAndNeverItsValue(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	created := owner(t, s)

	if err := s.Record(t.Context(), store.Audited{
		OperatorID: created.ID,
		Action:     "app.config.set",
		Target:     "shopfront",
		ConfigKey:  "DATABASE_URL",
	}); err != nil {
		t.Fatalf("recording a configuration change: %v", err)
	}

	entries, err := s.AuditEntries(t.Context(), 0)
	if err != nil {
		t.Fatalf("reading the audit trail: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the trail holds %d entries, want 1", len(entries))
	}
	if entries[0].ConfigKey != "DATABASE_URL" {
		t.Errorf("the entry names the key %q, want DATABASE_URL", entries[0].ConfigKey)
	}

	// [store.Audited] has nowhere to put a value, so this asserts the type
	// itself rather than only this one recorded entry.
	for _, field := range reflect.VisibleFields(reflect.TypeOf(store.Audited{})) {
		if strings.Contains(strings.ToLower(field.Name), "value") {
			t.Errorf("store.Audited has a %s field: an audit entry records which key changed, never its value",
				field.Name)
		}
	}
	for _, field := range reflect.VisibleFields(reflect.TypeOf(store.AuditEntry{})) {
		if strings.Contains(strings.ToLower(field.Name), "value") {
			t.Errorf("store.AuditEntry has a %s field: an audit entry records which key changed, never its value",
				field.Name)
		}
	}
}

func TestThereIsNoWayToChangeOrRemoveAnAuditEntry(t *testing.T) {
	t.Parallel()

	// Immutability that only the current code honours is not immutability. The
	// package must export no call that edits or deletes the trail, so the
	// surface itself is what is asserted.
	forbidden := []string{"update", "delete", "remove", "edit", "purge", "clear", "set", "revoke", "drop"}

	storeType := reflect.TypeOf(&store.Store{})
	for i := range storeType.NumMethod() {
		method := storeType.Method(i)
		lowered := strings.ToLower(method.Name)
		if !strings.Contains(lowered, "audit") {
			continue
		}
		for _, verb := range forbidden {
			if strings.Contains(lowered, verb) {
				t.Errorf("store.Store has a %s method: an audit entry is written once and never changed",
					method.Name)
			}
		}
	}

	// And the trail survives the operator it accuses, rather than being taken
	// down with them.
	s, _ := openStore(t)
	created := owner(t, s)

	invited, err := s.CreateOperator(t.Context(), "invited@example.test", "Invited", hash("invited"))
	if err != nil {
		t.Fatalf("creating an operator: %v", err)
	}
	if err := s.Record(t.Context(), store.Audited{
		OperatorID: invited.ID,
		Action:     "app.removed",
		Target:     "shopfront",
	}); err != nil {
		t.Fatalf("recording an audit entry: %v", err)
	}
	if err := s.RemoveOperator(t.Context(), invited.ID); err != nil {
		t.Fatalf("removing the operator: %v", err)
	}

	entries, err := s.OperatorAuditEntries(t.Context(), "invited@example.test", 0)
	if err != nil {
		t.Fatalf("reading what the removed operator did: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the removed operator's trail holds %d entries, want the 1 they left", len(entries))
	}
	if entries[0].OperatorEmail != "invited@example.test" {
		t.Errorf("the entry names %q, want the address the operator had", entries[0].OperatorEmail)
	}
	if entries[0].OperatorID != 0 {
		t.Errorf("the entry still points at the operator %d, want the link cleared", entries[0].OperatorID)
	}

	// The owner's own trail is untouched by any of it.
	if entries, err = s.OperatorAuditEntries(t.Context(), created.Email, 0); err != nil {
		t.Fatalf("reading the owner's trail: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the owner's trail holds %d entries, want none", len(entries))
	}
}

func TestTheDatabaseLivesWhereHostpathsSaysAndNowhereElse(t *testing.T) {
	t.Parallel()

	// A path spelled out in this package instead would be a second answer to
	// the question the uninstall contract needs exactly one answer to.
	if want := hostpaths.DataDir + "/dokkup.db"; hostpaths.DB != want {
		t.Errorf("hostpaths.DB = %q, want %q", hostpaths.DB, want)
	}
	if !strings.HasPrefix(hostpaths.DB, hostpaths.DataDir+"/") {
		t.Errorf("hostpaths.DB = %q, which is not inside the data directory removal visits", hostpaths.DB)
	}
}
