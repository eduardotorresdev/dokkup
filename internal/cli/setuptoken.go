package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/secret"
	"github.com/eduardotorresdev/dokkup/internal/store"
)

// TokenLifetime is how long a Setup Token is good for.
//
// Short-lived is half of what ADR-0007 buys: the token is printed into a
// terminal and stays in that scrollback forever, so what makes printing it
// acceptable is that the thing left behind stops working. Thirty minutes is
// long enough to open a browser, click past a self-signed certificate warning
// and choose a password without hurrying, and short enough that an operator who
// walked away from the session has not left a standing key to the host.
//
// A longer default was rejected on that second point, and a shorter one because
// the failure it produces -- a token that expired while the operator was
// reading the warning -- teaches people to reissue reflexively, which is the
// habit least likely to notice a refusal.
const TokenLifetime = 30 * time.Minute

// setupTokenIssuedAction is the audit vocabulary for minting one, fixed here so
// that the installer and the command cannot spell it two ways.
const setupTokenIssuedAction = "setup-token.issued" //nolint:gosec // G101: the audit vocabulary, not a credential

// setupTokenIssued is a token that exists, and the instant it stops existing.
//
// It goes no further than the terminal it is printed into: nothing holds one in
// a field that outlives a command, and there is deliberately no method that
// renders it into a log line. The hash is not here at all, because after
// [store.Store.IssueSetupToken] has returned there is nothing left to do with
// it, and a value nobody carries is a value nobody leaks.
type setupTokenIssued struct {
	token   string
	expires time.Time
}

func runSetupToken(env Env, args []string) error {
	fs := newFlagSet(env, "setup-token")
	db := fs.String("db", hostpaths.DB, "the database to issue the token into")
	ttl := fs.Duration("ttl", TokenLifetime, "how long the token is good for")

	if err := parseFlags(fs, args); err != nil {
		return err
	}
	// Refused here rather than left to the store, which would say the same
	// thing about an instant in the past without being able to say which flag
	// put it there.
	if *ttl <= 0 {
		return fmt.Errorf("--ttl is %s, so the token would be expired before it was printed", *ttl)
	}

	issued, err := setupTokenIssue(context.Background(), env, setupTokenThisHost(), *db, *ttl)
	if err != nil {
		// The refusal is the error and not a paragraph printed before one,
		// because Run already prefixes what comes back with the command name:
		// printing both would tell the operator the same thing twice and leave
		// them working out whether it was one refusal or two.
		//
		// The sentinel is carried through with %w rather than only its words,
		// because "there is already an owner" and "the disk is broken" are the
		// same exit code and must not become distinguishable only by matching
		// substrings -- the installer's report branches on exactly this.
		if errors.Is(err, store.ErrOwnerExists) {
			return fmt.Errorf("%w, so nothing was issued and %s is unchanged: a token now "+
				"would be a way to take that account over without signing in to it. Sign in "+
				"as the owner instead", store.ErrOwnerExists, *db)
		}
		return err
	}

	// The blank line is because this block is indented like a section of the
	// installation report, and run on its own it would otherwise begin flush
	// against the shell prompt with nothing separating the token from it.
	printf(env.Stdout, "\n")
	setupTokenPrint(env.Stdout, issued, "")
	return nil
}

// setupTokenIssue mints a token against the database at path and leaves that
// database in a state the service can go on using.
//
// The two halves are one call because the second is only ever needed on account
// of the first: `dokkup setup-token` is run with sudo, and root touching a file
// is what puts it out of the service's reach.
func setupTokenIssue(ctx context.Context, env Env, on setupTokenHost,
	path string, ttl time.Duration) (setupTokenIssued, error) {
	issued, err := setupTokenMint(ctx, path, ttl)
	if err != nil {
		return setupTokenIssued{}, err
	}

	// After the store is closed, and not on a store that is open: closing the
	// last connection is what checkpoints the write-ahead log and removes the
	// sidecars, so a chown before it would fix the ownership of files SQLite is
	// about to delete and recreate as root anyway.
	setupTokenGiveDatabaseToTheService(env.Stderr, on, path)
	return issued, nil
}

// setupTokenMint is the part that touches the database, opening and closing it
// around the two writes that have to both happen.
func setupTokenMint(ctx context.Context, path string, ttl time.Duration) (setupTokenIssued, error) {
	s, err := store.Open(ctx, path)
	if err != nil {
		return setupTokenIssued{}, fmt.Errorf("opening %s to issue a setup token: %w", path, err)
	}

	issued, err := setupTokenWrite(ctx, s, ttl)

	// Closed on every path, and a failure to close is only worth reporting when
	// nothing worse already went wrong: an uncheckpointed log is a smaller
	// thing than the reason the issue failed, and the operator can only act on
	// one message.
	if closeErr := s.Close(); closeErr != nil && err == nil {
		return setupTokenIssued{}, fmt.Errorf("closing %s after issuing a setup token: %w", path, closeErr)
	}
	if err != nil {
		return setupTokenIssued{}, err
	}
	return issued, nil
}

// setupTokenWrite mints the token and records that it was minted.
//
// An audit entry that could not be written fails the whole issue, and the token
// already in the database is left unprinted rather than shown. That way round
// because the two outcomes are not equally bad: a live token nobody was shown
// is spent by nobody and replaced by the next issue, whereas a token that was
// printed and that the trail has no record of is a credential to this host that
// the trail says was never created.
func setupTokenWrite(ctx context.Context, s *store.Store, ttl time.Duration) (setupTokenIssued, error) {
	token, hash, err := secret.NewToken()
	if err != nil {
		return setupTokenIssued{}, err
	}

	stored, err := s.IssueSetupToken(ctx, hash, time.Now().Add(ttl))
	if err != nil {
		return setupTokenIssued{}, err
	}

	// Nothing about the token goes in: no hash, no expiry-bearing target, not
	// even a note of which terminal asked. The trail is kept forever and read
	// by everyone who can read the database, and "one was issued, at this
	// instant" is the whole of what a reader needs to spot an issue nobody
	// remembers making.
	if err := s.RecordUnauthenticated(ctx, store.Audited{Action: setupTokenIssuedAction}); err != nil {
		return setupTokenIssued{}, err
	}

	// The expiry comes back off the store rather than from the time.Now above,
	// so that what the operator is told matches what will be compared against
	// in SQL down to the millisecond the store rounds to.
	return setupTokenIssued{token: token, expires: stored.ExpiresAt}, nil
}

// setupTokenGiveDatabaseToTheService hands the database back to the account the
// service runs as, after root has been in it.
//
// Whoever creates a file owns it. `dokkup setup-token` is run with sudo and the
// service runs as an unprivileged account (ADR-0005), so on a host where the
// service has not opened the database yet -- a fresh install, or an operator
// reissuing before ever signing in -- root creates the one file dokkup must be
// able to write, and the service is locked out of its own state at the next
// write. The same goes for the "-wal" and "-shm" files, which are created
// beside it and are as necessary as the database itself while they exist.
//
// The directory is in the list and not only the files in it, because
// [store.Open] does MkdirAll and a chmod to 0700 on the directory holding the
// database. Root creating that leaves the service without even the traversal
// bit, which is the same lockout arriving one level up -- and it is not
// hypothetical for a --db somewhere of the operator's choosing, or for a
// /var/lib/dokkup that was removed and recreated by this very run.
//
// Nothing here is fatal, and that is deliberate: the token has already been
// minted, and a run that returned an error at this point would look to the
// operator, and to anything scripting them, exactly like a run that issued
// nothing. So this says what is wrong and what to type, and lets the token be
// printed.
func setupTokenGiveDatabaseToTheService(w io.Writer, on setupTokenHost, path string) {
	// Not root means nothing was created as root, so there is nothing to give
	// back. This is also every developer running the command against a copy on
	// their own machine, who must not be told about an account they do not have.
	if on.geteuid() != 0 {
		return
	}

	uid, gid, err := on.account()
	if err != nil {
		printf(w, "\nThis host has no %s account yet, so %s is left owned by root.\n"+
			"That is expected before 'dokkup install' has run. If dokkup is installed and\n"+
			"this appears, the service cannot write its own database: %v\n",
			hostpaths.User, path, err)
		return
	}

	// The directory first, so that a run which fails part way through has moved
	// the outermost thing rather than the innermost: a directory the service
	// can enter holding files it cannot write is a state its own next open
	// corrects, and the reverse is not.
	for _, file := range []string{filepath.Dir(path), path, path + "-wal", path + "-shm"} {
		// A missing sidecar is the ordinary case, not a problem: SQLite removes
		// both when the last connection closes, which the mint above just did.
		if err := on.chown(file, uid, gid); err != nil && !errors.Is(err, fs.ErrNotExist) {
			printf(w, "\nCould not give %s to %s:%s, so the service may not be able to write it: %v\n"+
				"Fix it with: chown %s:%s %s\n",
				file, hostpaths.User, hostpaths.Group, err, hostpaths.User, hostpaths.Group, file)
		}
	}
}

// setupTokenHost is the part of the host handing the database back touches:
// which account this process is, which account the service is, and the syscall
// that moves a file between them.
//
// It is injected rather than called through [os] directly because every line of
// it runs only as root, only on a real host, and only at the moment where
// getting it wrong locks the service out of its own database -- which is to say
// it is the code least likely to be exercised and worst to have wrong. Three
// function fields are what make all four outcomes -- not root, no such account,
// a chown that was refused, and the ordinary one -- ordinary tests.
type setupTokenHost struct {
	geteuid func() int
	account func() (uid, gid int, err error)
	chown   func(path string, uid, gid int) error
}

// setupTokenThisHost is the real host, which is what both callers pass.
func setupTokenThisHost() setupTokenHost {
	return setupTokenHost{
		geteuid: os.Geteuid,
		account: setupTokenServiceAccount,
		chown:   os.Chown,
	}
}

// setupTokenServiceAccount resolves the numeric identity chown takes.
//
// The user and the group are looked up separately, rather than the group being
// taken as the user's primary one, because [hostpaths] names both and this must
// fail loudly if a host has them apart instead of quietly writing an ownership
// nobody asked for. It carries the caveat [host.Ops.LookupUser] documents: with
// CGO_ENABLED=0 this reads /etc/passwd and /etc/group alone.
func setupTokenServiceAccount() (uid, gid int, err error) {
	account, err := user.Lookup(hostpaths.User)
	if err != nil {
		return 0, 0, fmt.Errorf("looking up the %s account: %w", hostpaths.User, err)
	}
	group, err := user.LookupGroup(hostpaths.Group)
	if err != nil {
		return 0, 0, fmt.Errorf("looking up the %s group: %w", hostpaths.Group, err)
	}
	if uid, err = strconv.Atoi(account.Uid); err != nil {
		return 0, 0, fmt.Errorf("reading the uid of %s: %w", hostpaths.User, err)
	}
	if gid, err = strconv.Atoi(group.Gid); err != nil {
		return 0, 0, fmt.Errorf("reading the gid of %s: %w", hostpaths.Group, err)
	}
	return uid, gid, nil
}

// setupTokenPrint shows the token and says how to spend it. where is the
// address this dokkup is reached at when the caller knows it, which the
// installer does and a reissue on its own does not.
//
// One function for both, so that the sentence explaining why a secret is on
// screen is written once: an operator who reissues a token must be told the
// same thing as one who has just installed, and a second copy of this paragraph
// is a second copy to keep true.
func setupTokenPrint(w io.Writer, issued setupTokenIssued, where string) {
	if where != "" {
		printf(w, "  Create the owner account at %s, with this token:\n\n", where)
	} else {
		printf(w, "  Create the owner account with this token:\n\n")
	}
	printf(w, "    %s\n\n", issued.token)

	// The deadline is printed as an instant and as a duration. The instant is
	// what a person compares against a clock an hour later when they come back
	// to a terminal; the duration is what they act on now.
	printf(w, "  It can be spent once and stops working at %s,\n",
		issued.expires.Local().Format(time.RFC3339))
	printf(w, "  %s from now. That is what makes it safe enough to leave in this\n",
		time.Until(issued.expires).Round(time.Minute))
	printf(w, "  terminal's scrollback: what stays behind is a credential with one use\n")
	printf(w, "  and a deadline, not a password.\n\n")
	// Said because reissuing is a revocation, which is not what "issue another"
	// sounds like: there is at most one live token, and an operator who runs
	// the command again while a colleague has the first one on screen has just
	// stopped that one working. Finding that out from the failure is worse than
	// reading it here.
	printf(w, "  'dokkup setup-token' issues another and revokes this one, and refuses\n")
	printf(w, "  once the owner exists.\n\n")
}
