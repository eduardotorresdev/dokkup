package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultTimeout bounds a single invocation. Every shadow-utils tool here
// answers in milliseconds; the one that does not is [Tools.ProveSudo], which
// runs a real `dokku version` through runuser and sudo. Thirty seconds is what
// internal/dokku already allows a Dokku command, and this is one.
const defaultTimeout = 30 * time.Second

// waitDelay is how long Wait may go on waiting for output after the process it
// started is gone.
//
// Without it, defaultTimeout is advisory: killing the process on a context
// deadline does not close the stdout and stderr pipes, and anything that
// inherited them keeps Wait blocked. internal/dokku carries the measurement and
// the full explanation. The same bound belongs here because [Tools.ProveSudo]
// is exactly that case -- it runs the dokku shell script, which forks docker,
// git and plugin hooks -- and because an installation that hangs holds a host
// half-installed for as long as the operator is willing to wait for it.
const waitDelay = time.Second

// Tools drives the host's own programs on the local machine.
//
// Commands are built as argument vectors and executed directly. As in
// internal/dokku and internal/service, there is no shell in this file and there
// must never be one: a host program taking an operator-supplied name must not
// be able to read it as shell syntax.
type Tools struct {
	// Bin overrides a program's path, keyed by the constants above. Nil means
	// the constants themselves. Tests that need a stub program set it; the real
	// host never does.
	Bin map[string]string

	// Timeout bounds a single invocation. Zero means [defaultTimeout].
	Timeout time.Duration
}

var _ Ops = (*Tools)(nil)

// NewTools returns host operations driving the programs at their usual paths.
func NewTools() *Tools {
	return &Tools{Timeout: defaultTimeout}
}

// CommandError describes a host program that failed.
//
// It is duplicated from internal/dokku and internal/service on purpose, exactly
// as those two duplicate each other: the three seams stay independently
// readable, and a shared error type would have to name a program none of them
// agree on.
type CommandError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("%s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
	}
	return fmt.Sprintf("%s: %v", strings.Join(e.Args, " "), e.Err)
}

func (e *CommandError) Unwrap() error { return e.Err }

func (t *Tools) path(program string) string {
	if override, ok := t.Bin[program]; ok {
		return override
	}
	return program
}

// run invokes program with args.
//
// Standard output is discarded, unlike its counterparts in internal/dokku and
// internal/service, because nothing here answers through it. Measured on the
// devenv with the streams separated: a rule that does not parse puts its whole
// caret-pointed message on stderr and prints nothing at all on stdout, and what
// stdout carries on success -- `/tmp/zzhost-probe-good: parsed OK`, `nginx:
// configuration file ... test is successful` -- is a restatement of exit 0.
func (t *Tools) run(ctx context.Context, program string, args ...string) error {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	path := t.path(program)

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stderr = &stderr

	// None of these programs is interactive as dokkup invokes it, and one that
	// blocked reading standard input would hold an installation open until the
	// timeout with a host half-changed.
	cmd.Stdin = nil

	// See the WaitDelay comment in internal/dokku/exec.go for the measurement:
	// without this the timeout above is a suggestion, because Wait goes on
	// waiting for pipes that every process the child forked is still holding.
	cmd.WaitDelay = waitDelay

	if err := cmd.Run(); err != nil {
		return &CommandError{
			Args:   append([]string{path}, args...),
			Stderr: strings.TrimSpace(stderr.String()),
			Err:    err,
		}
	}
	return nil
}

// exitStatus reports the status a host program exited with, or -1 when it never
// ran at all. The shadow-utils tools answer through it: 6, 8 and 9 each mean
// something specific and each is mapped where it is read.
func exitStatus(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// account converts the strings os/user hands back. Uid and Gid are decimal on
// every Unix, and a host whose passwd database says otherwise is one dokkup
// should refuse rather than guess at.
func account(name, uid, gid string) (Account, error) {
	n, err := strconv.Atoi(uid)
	if err != nil {
		return Account{}, fmt.Errorf("reading the uid of %q: %w", name, err)
	}
	g, err := strconv.Atoi(gid)
	if err != nil {
		return Account{}, fmt.Errorf("reading the gid of %q: %w", name, err)
	}
	return Account{Name: name, Uid: n, Gid: g}, nil
}

// LookupUser implements [Ops].
func (t *Tools) LookupUser(_ context.Context, name string) (Account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		// The gate on useradd is this lookup rather than useradd's own exit 9,
		// because "the account is already there" is a fact installation needs
		// before it decides what its unwinding is allowed to remove.
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return Account{}, fmt.Errorf("%w: %s", ErrNoSuchUser, name)
		}
		return Account{}, err
	}
	return account(u.Username, u.Uid, u.Gid)
}

// LookupGroup implements [Ops].
func (t *Tools) LookupGroup(_ context.Context, name string) (Account, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		var unknown user.UnknownGroupError
		if errors.As(err, &unknown) {
			return Account{}, fmt.Errorf("%w: %s", ErrNoSuchGroup, name)
		}
		return Account{}, err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return Account{}, fmt.Errorf("reading the gid of %q: %w", name, err)
	}

	// A group names no owner, and -1 is what chown(2) reads as "leave the owner
	// alone". Zero would be root, so the one thing this field must never do --
	// hand a directory to root because a caller passed the wrong Account -- is
	// impossible rather than merely unlikely.
	return Account{Name: g.Name, Uid: -1, Gid: gid}, nil
}

// EnsureGroup implements [Ops].
//
// -f is the whole reason this needs no pre-check and no exit-code case.
// Measured: `groupadd --system zz-probe-acct` exits 0, and the second run
// prints `groupadd: group 'zz-probe-acct' already exists` and exits 9, while
// `groupadd -f --system zz-probe-acct` exits 0 with no output and leaves the
// group as it was. Adding a lookup here would buy a race where -f buys none.
func (t *Tools) EnsureGroup(ctx context.Context, name string) error {
	return t.run(ctx, Groupadd, "-f", "--system", name)
}

// EnsureUser implements [Ops].
//
// useradd is the only step of an installation that is not idempotent -- it has
// no -f -- so it is gated on the lookup, and its exit 9 is accepted as success
// as well. Measured: a repeat run prints `useradd: user 'zz-probe-acct' already
// exists` and exits 9. Both are needed and neither is redundant: the lookup is
// what tells the caller whether this run created the account, which is what
// decides whether unwinding may remove it, and the exit 9 covers the window
// between the lookup and the call. An account that was already there is
// reported as not created, so a failed installation never removes a user it
// found.
//
// The usermod that follows is unconditional. It is what reconciles an account a
// previous installation left with the wrong shell or home, and it costs
// nothing when there is nothing to do: measured, `usermod --shell
// /usr/sbin/nologin --home /nonexistent zz-probe-acct` prints `usermod: no
// changes` and exits 0.
func (t *Tools) EnsureUser(ctx context.Context, spec UserSpec) (bool, error) {
	created := false

	if _, err := t.LookupUser(ctx, spec.Name); err != nil {
		if !errors.Is(err, ErrNoSuchUser) {
			return false, err
		}

		// --gid names the primary group rather than letting USERGROUPS_ENAB
		// invent one, because that is the arrangement under which userdel later
		// takes the group with it, and --home-dir /nonexistent
		// --no-create-home creates no directory anywhere while leaving the
		// password field `!`, which `passwd -S` reports as locked.
		err := t.run(ctx, Useradd, "--system", "--gid", spec.Group,
			"--shell", spec.Shell, "--home-dir", spec.Home,
			"--no-create-home", spec.Name)
		switch {
		case err == nil:
			created = true
		case exitStatus(err) == 9:
			// The account appeared between the lookup and this call. It is
			// there, which is what was wanted, and this run did not make it.
		default:
			return false, err
		}
	}

	if err := t.run(ctx, Usermod, "--shell", spec.Shell, "--home", spec.Home, spec.Name); err != nil {
		return created, err
	}
	return created, nil
}

// JoinGroup implements [Ops].
//
// -aG, never -G. Measured on the devenv: with the account already in
// zz-probe-extra and dokku, `usermod -G dokku zz-probe-acct` exits 0 and leaves
// `groups=995(zz-probe-acct),1001(dokku)` -- zz-probe-extra is simply gone.
// Both forms exit 0, so nothing here would report the damage; dropping the `a`
// would silently take away memberships an operator gave the account for their
// own reasons.
//
// -aG is additive and idempotent, so this runs unconditionally with no
// pre-check. Exit 6 is "the user or the group does not exist", which on a Dokku
// host means the dokku group is missing, i.e. Dokku is not installed here.
// usermod's own stderr names which one -- `usermod: group
// 'zz-probe-nosuchgroup' does not exist` -- so it is carried through rather
// than replaced.
func (t *Tools) JoinGroup(ctx context.Context, user, group string) error {
	err := t.run(ctx, Usermod, "-aG", group, user)
	if exitStatus(err) == 6 {
		return fmt.Errorf("%w: %w", ErrNoSuchGroup, err)
	}
	return err
}

// EnsureDir implements [Ops].
//
// The chmod and the chown are not belt and braces, and neither is conditional.
// MkdirAll on a directory that already exists returns nil and changes neither
// mode nor owner -- measured: a second MkdirAll with 0700 over a 0750 directory
// left it 0750 and still owned by uid 996 -- so without them a directory an
// earlier installation got wrong stays wrong for ever. And MkdirAll's mode
// argument is a request, not an instruction: the process umask is 0022 on this
// host, which silently strips bits from it, measured as `mkdir -p` asking for
// 0770 and getting 0755.
func (t *Tools) EnsureDir(_ context.Context, path string, mode fs.FileMode, owner Account) error {
	if err := os.MkdirAll(path, mode); err != nil { //nolint:gosec // the caller's mode is the one the operator is promised
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil { //nolint:gosec // the caller's mode is the one the operator is promised
		return fmt.Errorf("setting the mode of %s: %w", path, err)
	}
	if err := os.Chown(path, owner.Uid, owner.Gid); err != nil {
		return fmt.Errorf("giving %s to %s: %w", path, owner.Name, err)
	}
	return nil
}

// WriteSudoers implements [Ops].
//
// The file is staged under a dotfile sibling, validated there, and only then
// renamed into place. Dotfiles in /etc/sudoers.d are ignored by both sudo and
// `visudo -c` -- measured: a deliberately broken `.zz-probe-brokentmp` left
// `visudo -c` exiting 0 and every rule still working -- so the rule is never
// half-visible during the window, and the rename is within one directory and
// therefore atomic.
//
// Never rename an unvalidated file into place. On sudo 1.9.15p5 a syntax error
// in sudoers.d does not break sudo for anyone: it prints the error and skips
// the bad line. But it prints it on every sudo call by every user on the host,
// for ever; that recovery behaviour is version-specific; and Dokku itself
// shells through sudo constantly, so the noise would land in the middle of the
// operator's own deploys. The cost of validating first is one subprocess.
func (t *Tools) WriteSudoers(ctx context.Context, path, rule string) error {
	staging := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")

	if err := os.WriteFile(staging, []byte(rule), 0o440); err != nil { //nolint:gosec // sudo has to be able to read the rule it enforces
		return fmt.Errorf("staging the sudoers rule at %s: %w", staging, err)
	}

	// Both are set rather than requested. sudo rejects a sudoers.d file owned by
	// anyone but uid 0 and the rule then grants nothing -- measured: `sudo:
	// /etc/sudoers.d/zz-probe-acct is owned by uid 996, should be 0` -- and
	// WriteFile's permission argument is filtered by the umask the same way
	// MkdirAll's is. 0440 root:root sits inside the accepted set with margin;
	// 0666 and 0777 are rejected as world writable.
	if err := os.Chmod(staging, 0o440); err != nil { //nolint:gosec // sudo has to be able to read the rule it enforces
		_ = os.Remove(staging)
		return fmt.Errorf("setting the mode of %s: %w", staging, err)
	}
	// Only root can hand a file to root, and installation refuses to run as
	// anyone else, so a non-root process here is a test working inside a
	// t.TempDir() rather than a host being installed. The cost of skipping is
	// that the one line no test covers is the line every real run takes; the
	// cost of not skipping is that the staging, the validation and the rename
	// have no test at all.
	if os.Geteuid() == 0 {
		if err := os.Chown(staging, 0, 0); err != nil {
			_ = os.Remove(staging)
			return fmt.Errorf("giving %s to root: %w", staging, err)
		}
	}

	// visudo checks syntax and nothing else: measured, it parses a file naming a
	// user that does not exist and one at mode 0666, and says "parsed OK" to
	// both. Mode, ownership and the ordering that makes the user exist before
	// the rule names them are this function's and the installer's to get right.
	//
	// What it does catch it reports well, and all of it on stderr, so the whole
	// of it is passed through untouched. Measured with the streams separated,
	// on a rule missing its colon:
	//
	//	/tmp/zzhost-probe-broken:1:49: syntax error
	//	zzhost-probe ALL=(dokku) NOPASSWD /usr/bin/dokku
	//	                                                ^
	if err := t.run(ctx, Visudo, "-cf", staging); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("the sudoers rule does not parse, so it was not installed: %w", err)
	}

	if err := os.Rename(staging, path); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("installing the sudoers rule at %s: %w", path, err)
	}
	return nil
}

// ProveSudo implements [Ops].
//
// runuser, never su. `su -s /bin/sh dokkup -c ...` starts a `systemd --user`
// session that outlives the command -- measured: `335371 zz-prob+
// /usr/lib/systemd/systemd --user` and `335372 (sd-pam)` still running
// afterwards, which made a later `userdel` fail with exit 8. runuser leaves
// nothing behind: after the same command through it, no process remained under
// the account. An installer that verifies itself with su would arm the failure
// the matching uninstall then hits.
//
// The runas is explicit and the ceiling is the dokku account rather than root.
// Measured on the rule installation writes: `runuser -u zz-probe-runas -- sudo
// -n -u dokku /usr/bin/dokku apps:list` works, while `sudo -n /usr/bin/dokku
// version` (runas root) and `sudo -n -u dokku /bin/ls /` (another program) are
// both answered `sudo: a password is required`.
func (t *Tools) ProveSudo(ctx context.Context, user, runAs, program string, args ...string) error {
	argv := []string{"-u", user, "--", t.path(Sudo), "-n", "-u", runAs, program}
	argv = append(argv, args...)

	if err := t.run(ctx, Runuser, argv...); err != nil {
		return fmt.Errorf("%w: %w", ErrSudoRefused, err)
	}
	return nil
}

// RemoveUser implements [Ops].
//
// Exit 6 is "already gone" and is a success: measured, a userdel of an account
// that is not there prints `userdel: user 'zz-probe-acct' does not exist` and
// exits 6, which is what a second uninstall on the same host sees.
//
// Exit 8 is not. Measured with a unit still running under the account:
// `userdel: user zz-probe-svc is currently used by process 342857`, exit 8. The
// answer to it is `systemctl disable --now`, which removal does first, and it
// is never --force: --force exits 0 and removes the passwd entry while the
// processes go on running under a uid the host is now free to hand to somebody
// else.
func (t *Tools) RemoveUser(ctx context.Context, name string) (bool, error) {
	if err := t.run(ctx, Userdel, name); err != nil {
		switch exitStatus(err) {
		case 6:
			return false, nil
		case 8:
			return false, fmt.Errorf("%w: %w", ErrUserBusy, err)
		default:
			return false, err
		}
	}
	return true, nil
}

// RemoveGroup implements [Ops].
//
// Exit 6 here is the ordinary path, not a surprise. userdel removes the primary
// group by itself when the group shares the user's name and has no other
// members -- measured on all three arrangements: same name and no members, the
// group was gone; same name plus another member, `userdel: group zz-probe-acct
// not removed because it has other members` and the group survived; a
// differently named primary group, it survived. dokkup's user and group are
// both named "dokkup", so on nearly every uninstall `groupdel dokkup` exits 6
// because there is nothing left to remove, and reporting that as a failure
// would make a clean removal look broken.
//
// Exit 8 is "is the primary group of user X" and stays an error. Never --force:
// measured, `groupdel --force` exits 0 on a primary group and leaves `id`
// showing a bare numeric gid with no name behind it.
func (t *Tools) RemoveGroup(ctx context.Context, name string) (bool, error) {
	if err := t.run(ctx, Groupdel, name); err != nil {
		if exitStatus(err) == 6 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CheckProxy implements [Ops].
func (t *Tools) CheckProxy(ctx context.Context) error {
	return t.run(ctx, Nginx, "-t")
}

// wrapperConfig is the smallest nginx configuration that can include a vhost
// fragment and nothing else. It is the technique Dokku uses itself in
// templates/validate.conf.sigil, and every directive earns its place: `events`
// is mandatory or nginx refuses the file before it reaches the include, and the
// two logs are silenced so that testing a candidate cannot write into the logs
// the operator reads.
//
// Measured against a real candidate vhost on nginx 1.24, with the live
// configuration left untouched throughout:
//
//	good candidate      nginx: configuration file .../vhost.candidate.wrapper
//	                    test is successful                            exit 0
//	bad directive       [emerg] unknown directive "totally_bogus_directive"
//	                    in /tmp/zzhost-probe-nginx/vhost.candidate:1  exit 1
//	missing certificate [emerg] cannot load certificate ".../nope.pem":
//	                    BIO_new_file() failed                         exit 1
//
// The [emerg] names the candidate rather than the wrapper, so an operator is
// pointed at the file they can do something about.
const wrapperConfig = "events { worker_connections 32; }\n" +
	"http { access_log off; error_log /dev/null; include %s; }\n"

// CheckProxyFragment implements [Ops].
//
// The candidate is tested through a wrapper of its own rather than by being put
// in conf.d first, because what this catches has to be caught before anything
// enters /etc/nginx: a syntax error, and an ssl_certificate naming a file that
// is not there, which `nginx -t` fails outright on. Leaving a failing file in
// conf.d "for the operator to inspect" is the single most dangerous thing
// installation could do -- nginx.service carries `ExecStartPre=/usr/sbin/nginx
// -t -q`, so a broken fragment does not break a reload, it breaks the next
// start, which takes every app on the host down at the next reboot.
//
// It cannot catch what only the whole configuration knows: a duplicate
// default_server, or a server_name another vhost already claims.
// [Ops.CheckProxy] after the file is in place is what covers those, and neither
// replaces the other.
func (t *Tools) CheckProxyFragment(ctx context.Context, path string) error {
	wrapper := path + ".wrapper"

	//nolint:gosec // nginx reads this as root and there is nothing secret in it
	if err := os.WriteFile(wrapper, fmt.Appendf(nil, wrapperConfig, path), 0o644); err != nil {
		return fmt.Errorf("writing the configuration that tests %s: %w", path, err)
	}
	// Best effort: the wrapper sits beside a candidate the caller is about to
	// remove anyway, and a failure to tidy it up must not mask the answer.
	defer func() { _ = os.Remove(wrapper) }()

	return t.run(ctx, Nginx, "-t", "-c", wrapper)
}
