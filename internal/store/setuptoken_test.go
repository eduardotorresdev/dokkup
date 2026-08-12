package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/store"
)

// issue puts a token in the store and fails the test if it could not, since a
// case about redeeming has nothing to say until one exists.
func issue(t *testing.T, s *store.Store, of string, ttl time.Duration) string {
	t.Helper()

	if _, err := s.IssueSetupToken(t.Context(), hash(of), time.Now().Add(ttl)); err != nil {
		t.Fatalf("issuing the %s setup token: %v", of, err)
	}
	return hash(of)
}

// operatorCount is what the "and nothing was created" half of most of these
// cases is checked with. Counting rather than reading the owner, because the
// failure being guarded against is a second row appearing.
func operatorCount(t *testing.T, s *store.Store) int {
	t.Helper()

	operators, err := s.Operators(t.Context())
	if err != nil {
		t.Fatalf("reading the operators: %v", err)
	}
	return len(operators)
}

func TestASetupTokenIsRedeemedOnceAndIsThenNotAToken(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)

	if owned, err := s.HasOwner(t.Context()); err != nil {
		t.Fatalf("reading whether a fresh installation has an owner: %v", err)
	} else if owned {
		t.Fatal("a fresh installation reports that it already has an owner")
	}

	token := issue(t, s, "printed", time.Hour)

	created, err := s.RedeemSetupToken(t.Context(), token, "Owner@Example.test", "The Owner", hash("password"))
	if err != nil {
		t.Fatalf("redeeming a live setup token: %v", err)
	}
	if !created.IsOwner {
		t.Error("redeeming the setup token created an operator who is not the owner")
	}
	// Normalised on the way in, so that the address the Owner signs in with is
	// the address they typed at the setup screen however they capitalised it.
	if created.Email != "owner@example.test" {
		t.Errorf("the owner's email is %q, want it normalised to owner@example.test", created.Email)
	}
	if created.ID == 0 {
		t.Error("the owner came back without an id")
	}

	if owned, err := s.HasOwner(t.Context()); err != nil {
		t.Fatalf("reading whether the installation has an owner: %v", err)
	} else if !owned {
		t.Error("the installation reports no owner after one was created")
	}

	// The second redemption is the attack the single use exists for: the token
	// was printed into a terminal and may have been read by somebody else.
	_, err = s.RedeemSetupToken(t.Context(), token, "attacker@example.test", "Somebody", hash("password"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("redeeming a spent setup token = %v, want ErrNotFound", err)
	}
	if count := operatorCount(t, s); count != 1 {
		t.Errorf("the installation has %d operators after a second redemption, want the 1 that was created", count)
	}
}

func TestASetupTokenThatHasRunOutRedeemsNothing(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)

	// Short-lived is the point of the credential, so the expiry is exercised by
	// waiting one out rather than by writing a row behind the store's back.
	const ttl = 20 * time.Millisecond
	token := issue(t, s, "printed", ttl)
	time.Sleep(2 * ttl)

	_, err := s.RedeemSetupToken(t.Context(), token, "late@example.test", "Too Late", hash("password"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("redeeming an expired setup token = %v, want ErrNotFound", err)
	}
	if count := operatorCount(t, s); count != 0 {
		t.Errorf("an expired setup token created %d operators", count)
	}

	// An unknown hash is the same answer, which is what stops a caller telling
	// a token that never existed from one that has run out.
	_, err = s.RedeemSetupToken(t.Context(), hash("never issued"), "who@example.test", "Who", hash("password"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("redeeming a setup token that was never issued = %v, want ErrNotFound", err)
	}
}

func TestASetupTokenIsRefusedOnceTheInstallationHasAnOwner(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	owner(t, s)

	// ADR-0007: reissuing while an Owner exists would be an unauthenticated
	// path to taking the host over, which is the failure the token was
	// introduced to prevent.
	_, err := s.IssueSetupToken(t.Context(), hash("printed"), time.Now().Add(time.Hour))
	if !errors.Is(err, store.ErrOwnerExists) {
		t.Fatalf("issuing a setup token to an owned installation = %v, want ErrOwnerExists", err)
	}
}

func TestASetupTokenIssuedBeforeAnOwnerAppearedRedeemsNoSecondOwner(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	token := issue(t, s, "printed", time.Hour)

	// The Owner arrives by another route -- the installer's own redemption on
	// another connection -- after this token was printed. The live token must
	// not become a second owner.
	owner(t, s)

	_, err := s.RedeemSetupToken(t.Context(), token, "second@example.test", "Second", hash("password"))
	if !errors.Is(err, store.ErrOwnerExists) {
		t.Fatalf("redeeming a live setup token against an owned installation = %v, want ErrOwnerExists", err)
	}
	if count := operatorCount(t, s); count != 1 {
		t.Errorf("the installation has %d operators, want the 1 owner", count)
	}
}

func TestIssuingASetupTokenRevokesTheOneBeforeIt(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)

	first := issue(t, s, "first", time.Hour)
	second := issue(t, s, "second", time.Hour)

	// An operator who reissues because they suspect the first token was seen
	// has to end up with exactly one credential that works.
	_, err := s.RedeemSetupToken(t.Context(), first, "first@example.test", "First", hash("password"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("redeeming a setup token that was reissued over = %v, want ErrNotFound", err)
	}

	if _, err := s.RedeemSetupToken(t.Context(), second, "second@example.test", "Second", hash("password")); err != nil {
		t.Fatalf("redeeming the setup token that was issued last: %v", err)
	}
	if count := operatorCount(t, s); count != 1 {
		t.Errorf("the installation has %d operators, want the 1 the last token created", count)
	}
}

func TestSimultaneousRedemptionsOfOneSetupTokenCreateOneOwner(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	token := issue(t, s, "printed", time.Hour)

	// The token is spent by the same transaction that creates the Owner, so
	// this is the case that says so: a check followed by an insert would let
	// two of these through and hand the host to whichever operator lost the
	// race.
	const attempts = 8

	var (
		wait      sync.WaitGroup
		mu        sync.Mutex
		redeemed  []store.Operator
		unwelcome []error
	)
	for attempt := range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()

			created, err := s.RedeemSetupToken(context.Background(), token,
				emailOf(attempt), "Racer", hash("password"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				redeemed = append(redeemed, created)
			case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrOwnerExists):
				// Both are a redemption that lost, which is the answer this
				// test is asking for.
			default:
				unwelcome = append(unwelcome, err)
			}
		}()
	}
	wait.Wait()

	if len(unwelcome) > 0 {
		t.Errorf("%d of %d simultaneous redemptions failed for another reason: %v",
			len(unwelcome), attempts, unwelcome)
	}
	if len(redeemed) != 1 {
		t.Errorf("%d of %d simultaneous redemptions succeeded, want exactly 1", len(redeemed), attempts)
	}
	if count := operatorCount(t, s); count != 1 {
		t.Errorf("%d simultaneous redemptions left %d operators, want 1", attempts, count)
	}
}

// emailOf gives each racer its own address, so that two of them succeeding
// cannot be mistaken for one succeeding twice.
func emailOf(attempt int) string {
	return fmt.Sprintf("racer-%d@example.test", attempt)
}

func TestExpiredSetupTokensAreDroppedAndLiveOnesAreNot(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)

	const ttl = 20 * time.Millisecond
	issue(t, s, "expiring", ttl)
	time.Sleep(2 * ttl)

	dropped, err := s.DropExpiredSetupTokens(t.Context())
	if err != nil {
		t.Fatalf("dropping the expired setup tokens: %v", err)
	}
	if dropped != 1 {
		t.Errorf("dropping the expired setup tokens removed %d, want the 1 that had run out", dropped)
	}

	live := issue(t, s, "live", time.Hour)
	dropped, err = s.DropExpiredSetupTokens(t.Context())
	if err != nil {
		t.Fatalf("dropping the expired setup tokens with a live one outstanding: %v", err)
	}
	if dropped != 0 {
		t.Errorf("dropping the expired setup tokens removed %d, want none: a live token was outstanding", dropped)
	}
	if _, err := s.RedeemSetupToken(t.Context(), live, "owner@example.test", "The Owner", hash("password")); err != nil {
		t.Errorf("the live setup token no longer redeems after the housekeeping ran: %v", err)
	}
}

func TestASetupTokenNeedsAHashAndAnExpiryThatHasNotPassed(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)

	if _, err := s.IssueSetupToken(t.Context(), "", time.Now().Add(time.Hour)); err == nil {
		t.Error("issuing a setup token with no hash was accepted")
	}
	// A token that is dead on arrival is a caller bug, and storing it would
	// mean an installer printing a credential nobody can spend.
	if _, err := s.IssueSetupToken(t.Context(), hash("printed"), time.Now().Add(-time.Second)); err == nil {
		t.Error("issuing a setup token that had already expired was accepted")
	}

	token := issue(t, s, "printed", time.Hour)
	for _, missing := range []struct {
		what                string
		email, passwordHash string
	}{
		{"an email address", "  ", hash("password")},
		{"a password hash", "owner@example.test", ""},
	} {
		if _, err := s.RedeemSetupToken(t.Context(), token, missing.email, "The Owner", missing.passwordHash); err == nil {
			t.Errorf("redeeming a setup token without %s was accepted", missing.what)
		}
	}
	// Refused before anything was written, so the credential is still there
	// for the operator who mistyped at the setup screen.
	if _, err := s.RedeemSetupToken(t.Context(), token, "owner@example.test", "The Owner", hash("password")); err != nil {
		t.Errorf("redeeming after two refused attempts: %v", err)
	}
}

func TestARedemptionThatFailsHalfwayThroughSpendsNothing(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	token := issue(t, s, "printed", time.Hour)

	// An operator who is not the Owner already holds the address the
	// redemption will claim -- an installation whose database was restored
	// with the operators in it and the Owner row lost, which is the state this
	// row exists to reproduce. The INSERT then fails after the DELETE has
	// already run, and that is the only way to ask whether the two really are
	// one transaction: without it the token is spent and the installation has
	// no Owner and no way left to make one.
	if _, err := s.CreateOperator(t.Context(), "taken@example.test", "Invited", hash("invited")); err != nil {
		t.Fatalf("creating the operator who already holds the address: %v", err)
	}

	_, err := s.RedeemSetupToken(t.Context(), token, "Taken@example.test", "The Owner", hash("password"))
	if !errors.Is(err, store.ErrExists) {
		t.Fatalf("redeeming onto an address an operator already holds = %v, want ErrExists", err)
	}

	// The whole point: the failed insert rolled the delete back with it, so
	// the operator still holds the one credential the installer printed.
	created, err := s.RedeemSetupToken(t.Context(), token, "owner@example.test", "The Owner", hash("password"))
	if err != nil {
		t.Fatalf("redeeming after a redemption that failed on its insert: %v", err)
	}
	if !created.IsOwner {
		t.Error("the redemption that followed created an operator who is not the owner")
	}
}

func TestAnIssueThatIsRefusedLeavesTheOutstandingTokenAlone(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	token := issue(t, s, "printed", time.Hour)

	// The DELETE that clears the outstanding token is unconditional, so the
	// only thing keeping a refused issue from revoking a live credential is
	// that it rolls back. An operator who runs `dokkup setup-token` a second
	// time and is told the host already has an Owner must not have had their
	// still-valid token taken away by being told so.
	owner(t, s)
	if _, err := s.IssueSetupToken(t.Context(), hash("second"), time.Now().Add(time.Hour)); !errors.Is(err, store.ErrOwnerExists) {
		t.Fatalf("issuing a setup token to an owned installation = %v, want ErrOwnerExists", err)
	}

	// ErrOwnerExists and not ErrNotFound is what says the row is still there:
	// a spent or deleted token never reaches the insert that answers this.
	_, err := s.RedeemSetupToken(t.Context(), token, "second@example.test", "Second", hash("password"))
	if !errors.Is(err, store.ErrOwnerExists) {
		t.Errorf("redeeming the outstanding token after a refused issue = %v, want ErrOwnerExists: "+
			"the refused issue revoked it", err)
	}
}

func TestAnUnauthenticatedActIsRecordedWithNobodyNamed(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)

	if err := s.RecordUnauthenticated(t.Context(), store.Audited{Action: "setup-token.issued"}); err != nil {
		t.Fatalf("recording an unauthenticated audit entry: %v", err)
	}
	if err := s.RecordUnauthenticated(t.Context(), store.Audited{
		Action: "setup-token.rejected",
		Target: "attacker@example.test",
	}); err != nil {
		t.Fatalf("recording a rejected redemption: %v", err)
	}

	entries, err := s.AuditEntries(t.Context(), 0)
	if err != nil {
		t.Fatalf("reading the audit trail: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("the trail holds %d entries, want the 2 that were written", len(entries))
	}

	// Newest first, so the rejection is the one at the front.
	rejected, issued := entries[0], entries[1]
	if rejected.Action != "setup-token.rejected" || issued.Action != "setup-token.issued" {
		t.Fatalf("the trail reads %q then %q, want the rejection first", rejected.Action, issued.Action)
	}
	for _, entry := range entries {
		// NULL operator_id, which is what reads back as zero, and no email:
		// nobody was signed in, and the trail must not imply somebody was.
		if entry.OperatorID != 0 {
			t.Errorf("the %q entry names the operator %d, want nobody", entry.Action, entry.OperatorID)
		}
		if entry.OperatorEmail != "" {
			t.Errorf("the %q entry names %q, want no operator email", entry.Action, entry.OperatorEmail)
		}
		if entry.RecordedAt.IsZero() {
			t.Errorf("the %q entry was recorded at no instant", entry.Action)
		}
	}
	// What the attempt claimed is a claim, and it belongs in the target rather
	// than in the column that names an operator.
	if rejected.Target != "attacker@example.test" {
		t.Errorf("the rejected redemption targets %q, want the address it claimed", rejected.Target)
	}
	if issued.Target != "" {
		t.Errorf("issuing a token targets %q, want nothing", issued.Target)
	}
}

func TestAnUnauthenticatedEntryCannotNameAnOperatorOrNoAction(t *testing.T) {
	t.Parallel()

	s, _ := openStore(t)
	created := owner(t, s)

	if err := s.RecordUnauthenticated(t.Context(), store.Audited{Action: ""}); err == nil {
		t.Error("an unauthenticated audit entry naming no action was accepted")
	}
	// A caller with an operator in hand wants Record, and writing the entry
	// without them would drop the one attribution the trail could have made.
	if err := s.RecordUnauthenticated(t.Context(), store.Audited{
		OperatorID: created.ID,
		Action:     "setup-token.redeemed",
	}); err == nil {
		t.Error("an unauthenticated audit entry naming an operator was accepted")
	}

	entries, err := s.AuditEntries(t.Context(), 0)
	if err != nil {
		t.Fatalf("reading the audit trail: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the trail holds %d entries after two refusals, want none", len(entries))
	}
}
