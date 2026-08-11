package host_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/host"
	"github.com/eduardotorresdev/dokkup/internal/stubprog"
)

// programs is every host program this package can invoke. stubTools stands in
// for all of them at once, so a test that reaches for one it did not think
// about gets a recorded line rather than the real thing running as root.
var programs = []string{
	host.Groupadd, host.Useradd, host.Usermod, host.Userdel, host.Groupdel,
	host.Visudo, host.Runuser, host.Sudo, host.Nginx,
}

// stub is what a stand-in program does when it is called.
type stub struct {
	// exit is the status it exits with. The measured exit-code table is the
	// point of this file, so every case that matters sets it deliberately.
	exit int

	// stderr is written before exiting. Only one program's output is part of
	// the contract: visudo's caret-pointed syntax error is passed through to
	// the operator verbatim, so a test has to be able to supply one.
	stderr string

	// dumps appends the contents of the last argument to the log. It is how the
	// wrapper nginx is handed gets asserted at all, since the wrapper is
	// removed as soon as the check returns.
	dumps bool
}

// The argument vectors every test in this package recorded, so that [TestMain]
// can make one assertion over all of them at once.
var (
	allVectorsMu sync.Mutex
	allVectors   []string
)

// stubTools returns [host.Tools] whose programs are stand-in scripts that
// record the argument vector they were given and exit with a chosen status.
//
// It is how the measured exit-code table is tested on a machine that has no
// shadow-utils on it, and on one where running the real programs would create
// system accounts and edit /etc/sudoers.d. A program not named in stubs exits 0
// in silence.
func stubTools(t *testing.T, stubs map[string]stub) (*host.Tools, func() []string) {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, "argv")

	bin := make(map[string]string, len(programs))
	for _, program := range programs {
		this := stubs[program]
		name := filepath.Base(program)
		path := filepath.Join(dir, name)

		script := "#!/bin/sh\n" +
			"printf '%s %s\\n' '" + name + "' \"$*\" >> '" + log + "'\n"
		if this.dumps {
			script += "last=''\nfor a in \"$@\"; do last=\"$a\"; done\n" +
				"if [ -f \"$last\" ]; then cat \"$last\" >> '" + log + "'; fi\n"
		}
		if this.stderr != "" {
			script += "cat >&2 <<'HOST_STUB_EOF'\n" + this.stderr + "\nHOST_STUB_EOF\n"
		}
		script += "exit " + strconv.Itoa(this.exit) + "\n"

		if err := stubprog.Write(path, script); err != nil {
			t.Fatalf("writing the stub for %s: %v", program, err)
		}
		bin[program] = path
	}

	recorded := func() []string {
		content, err := os.ReadFile(log)
		if err != nil {
			return nil
		}
		var lines []string
		for line := range strings.SplitSeq(string(content), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, line)
			}
		}
		return lines
	}

	t.Cleanup(func() {
		allVectorsMu.Lock()
		defer allVectorsMu.Unlock()

		allVectors = append(allVectors, recorded()...)
	})

	return &host.Tools{Bin: bin}, recorded
}

// absentUser is a name this host does not have.
func absentUser(t *testing.T) string {
	t.Helper()

	name := "dokkup-absent-" + strconv.Itoa(os.Getpid())
	if _, err := user.Lookup(name); err == nil {
		t.Fatalf("this host has a user called %q, which no test here can work around", name)
	}
	return name
}

// TestMain runs the package's tests and then makes one assertion across all of
// them at once: no host program was ever passed --force.
//
// It is here rather than in a test of its own because the guarantee is about
// every call this package makes, and a test could only see the vectors of the
// tests the shuffle happened to put before it.
//
// --force exists on both removal tools and is the wrong answer to both measured
// failures. `userdel --force` exits 0 on an account something is still running
// as, removing the passwd entry while those processes go on under a uid the
// host is then free to hand to somebody else. `groupdel --force` exits 0 on a
// primary group and leaves `id` reporting a bare numeric gid with no name
// behind it.
func TestMain(m *testing.M) {
	code := m.Run()

	allVectorsMu.Lock()
	for _, vector := range allVectors {
		if strings.Contains(vector, "--force") {
			fmt.Fprintf(os.Stderr, "a host program was passed --force, which succeeds "+
				"while orphaning processes and leaving a dangling gid: %s\n", vector)
			code = 1
		}
	}
	allVectorsMu.Unlock()

	os.Exit(code)
}

func TestARepeatedGroupCreationIsNotAnError(t *testing.T) {
	t.Parallel()

	tools, recorded := stubTools(t, nil)

	for range 2 {
		if err := tools.EnsureGroup(t.Context(), "dokkup"); err != nil {
			t.Fatalf("EnsureGroup: %v", err)
		}
	}

	// -f is the only reason this is idempotent, and nothing downstream would
	// recover without it: plain `groupadd --system dokkup` exits 9 the second
	// time with `groupadd: group 'dokkup' already exists`.
	want := []string{"groupadd -f --system dokkup", "groupadd -f --system dokkup"}
	if got := recorded(); !slices.Equal(got, want) {
		t.Errorf("groupadd was asked %v, want %v", got, want)
	}
}

func TestAUserThatAlreadyExistsIsNotCreatedTwiceAndIsReconciled(t *testing.T) {
	t.Parallel()

	current, err := user.Current()
	if err != nil {
		t.Fatalf("resolving the account these tests run as: %v", err)
	}

	tools, recorded := stubTools(t, nil)

	created, err := tools.EnsureUser(t.Context(), host.UserSpec{
		Name:  current.Username,
		Group: current.Username,
		Shell: host.Nologin,
		Home:  "/nonexistent",
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	// An account this run did not create is one a failed run must not remove.
	if created {
		t.Error("an account that was already on the host was reported as created by this run")
	}

	// useradd has no -f and would exit 9; the reconciling usermod runs all the
	// same, because it is what corrects a shell or home an earlier installation
	// got wrong, and it costs nothing when there is nothing to do: measured,
	// `usermod --shell /usr/sbin/nologin --home /nonexistent zz-probe-acct`
	// prints `usermod: no changes` and exits 0.
	want := []string{"usermod --shell /usr/sbin/nologin --home /nonexistent " + current.Username}
	if got := recorded(); !slices.Equal(got, want) {
		t.Errorf("the host was asked %v, want %v", got, want)
	}
}

func TestAUserThatIsNotThereIsCreatedInTheGroupItWasGiven(t *testing.T) {
	t.Parallel()

	name := absentUser(t)
	tools, recorded := stubTools(t, nil)

	created, err := tools.EnsureUser(t.Context(), host.UserSpec{
		Name:  name,
		Group: "dokkup",
		Shell: host.Nologin,
		Home:  "/nonexistent",
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if !created {
		t.Error("an account this run created was not reported as created, so a failure would leave it behind")
	}

	// --gid names the primary group rather than letting USERGROUPS_ENAB invent
	// one: that is the arrangement under which userdel later takes the group
	// with it, measured as removing the primary group iff it shares the user's
	// name and has no other members.
	want := []string{
		"useradd --system --gid dokkup --shell /usr/sbin/nologin " +
			"--home-dir /nonexistent --no-create-home " + name,
		"usermod --shell /usr/sbin/nologin --home /nonexistent " + name,
	}
	if got := recorded(); !slices.Equal(got, want) {
		t.Errorf("the host was asked %v, want %v", got, want)
	}
}

// The account appearing between the lookup and the call is the one window the
// lookup cannot close, and useradd says so plainly: `useradd: user 'X' already
// exists`, exit 9. Reporting that as created would let a later failure remove
// an account this run did not make.
func TestAnAccountUseraddLostTheRaceForIsNotReportedAsCreated(t *testing.T) {
	t.Parallel()

	tools, _ := stubTools(t, map[string]stub{host.Useradd: {exit: 9}})

	created, err := tools.EnsureUser(t.Context(), host.UserSpec{
		Name:  absentUser(t),
		Group: "dokkup",
		Shell: host.Nologin,
		Home:  "/nonexistent",
	})
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if created {
		t.Error("an account useradd said already existed was reported as created by this run")
	}
}

func TestSupplementaryGroupsAreAddedToRatherThanReplaced(t *testing.T) {
	t.Parallel()

	tools, recorded := stubTools(t, nil)

	if err := tools.JoinGroup(t.Context(), "dokkup", "dokku"); err != nil {
		t.Fatalf("JoinGroup: %v", err)
	}

	// -G without the a is the bug this test exists for. Both forms exit 0, so
	// nothing at runtime would report the damage: measured, an account in
	// zz-probe-extra and dokku came out of `usermod -G dokku zz-probe-acct` as
	// `groups=995(zz-probe-acct),1001(dokku)` with zz-probe-extra simply gone.
	want := []string{"usermod -aG dokku dokkup"}
	if got := recorded(); !slices.Equal(got, want) {
		t.Errorf("usermod was asked %v, want %v", got, want)
	}
}

// Exit 6 from usermod is a precondition failure rather than a command that went
// wrong, and it is the one an operator can act on: there is no dokku group
// here, so Dokku is not installed on this host.
func TestAMissingDokkuGroupIsReportedAsAnAbsentGroup(t *testing.T) {
	t.Parallel()

	const missing = "usermod: group 'dokku' does not exist"

	tools, _ := stubTools(t, map[string]stub{host.Usermod: {exit: 6, stderr: missing}})

	err := tools.JoinGroup(t.Context(), "dokkup", "dokku")
	if !errors.Is(err, host.ErrNoSuchGroup) {
		t.Fatalf("JoinGroup = %v, want %v", err, host.ErrNoSuchGroup)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("the error does not say which identity is missing: %v", err)
	}
}

// The measured refusal, and the one removal must never answer with --force:
// `userdel: user zz-probe-svc is currently used by process 342857`, exit 8. The
// answer is to stop the service, which removal does first.
func TestRemovingAUserSomethingIsStillRunningAsIsAnErrorRatherThanForced(t *testing.T) {
	t.Parallel()

	const busy = "userdel: user dokkup is currently used by process 342857"

	tools, recorded := stubTools(t, map[string]stub{host.Userdel: {exit: 8, stderr: busy}})

	removed, err := tools.RemoveUser(t.Context(), "dokkup")
	if !errors.Is(err, host.ErrUserBusy) {
		t.Fatalf("RemoveUser = %v, want %v", err, host.ErrUserBusy)
	}
	if removed {
		t.Error("a user that is still in use was reported as removed")
	}
	if !strings.Contains(err.Error(), busy) {
		t.Errorf("the error does not name what is still running: %v", err)
	}

	want := []string{"userdel dokkup"}
	if got := recorded(); !slices.Equal(got, want) {
		t.Errorf("userdel was asked %v, want %v", got, want)
	}
}

// Because dokkup's user and group share a name, userdel removes the group by
// itself and this exit 6 is what nearly every uninstall sees. Reporting it as a
// failure would make a clean removal look broken.
func TestAGroupThatIsAlreadyGoneIsASuccessBecauseUserdelTookIt(t *testing.T) {
	t.Parallel()

	const gone = "groupdel: group 'dokkup' does not exist"

	tools, _ := stubTools(t, map[string]stub{host.Groupdel: {exit: 6, stderr: gone}})

	removed, err := tools.RemoveGroup(t.Context(), "dokkup")
	if err != nil {
		t.Fatalf("RemoveGroup: %v", err)
	}
	if removed {
		t.Error("a group that was already gone was reported as removed by this run")
	}
}

func TestTheExitStatusOfARemovalToolIsAnAnswerRatherThanAFailure(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		program string
		remove  func(*host.Tools, context.Context, string) (bool, error)
		exit    int
		removed bool
		fails   bool
	}{
		"the user was there and is not any more": {
			program: host.Userdel, remove: (*host.Tools).RemoveUser, exit: 0, removed: true,
		},
		// The second uninstall on the same host, which must not complain.
		"the user was already gone": {
			program: host.Userdel, remove: (*host.Tools).RemoveUser, exit: 6,
		},
		"the group was there and is not any more": {
			program: host.Groupdel, remove: (*host.Tools).RemoveGroup, exit: 0, removed: true,
		},
		// `groupdel: cannot remove the primary group of user 'X'`. Something
		// still holds it, and --force would leave a dangling numeric gid.
		"the group is still somebody's primary group": {
			program: host.Groupdel, remove: (*host.Tools).RemoveGroup, exit: 8, fails: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tools, _ := stubTools(t, map[string]stub{tc.program: {exit: tc.exit}})

			removed, err := tc.remove(tools, t.Context(), "dokkup")
			if tc.fails != (err != nil) {
				t.Fatalf("error = %v, want an error: %v", err, tc.fails)
			}
			if removed != tc.removed {
				t.Errorf("removed = %v, want %v", removed, tc.removed)
			}
		})
	}
}

// The measured output of `visudo -cf` on a rule missing its colon, caret and
// all, taken on the devenv with stdout and stderr separated. It is quoted
// rather than paraphrased because passing it through untouched is the contract:
// it is the only thing that tells an operator where the rule is wrong.
const brokenSudoers = `/tmp/zzhost-probe-broken:1:49: syntax error
zzhost-probe ALL=(dokku) NOPASSWD /usr/bin/dokku
                                                ^`

func TestASudoersFileThatDoesNotParseNeverReachesSudo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "dokkup")
	staging := filepath.Join(dir, ".dokkup.tmp")

	tools, _ := stubTools(t, map[string]stub{host.Visudo: {exit: 1, stderr: brokenSudoers}})

	err := tools.WriteSudoers(t.Context(), path, "dokkup ALL=(dokku) NOPASSWD /usr/bin/dokku\n")
	if err == nil {
		t.Fatal("a sudoers rule that does not parse was installed anyway")
	}

	// On sudo 1.9.15p5 a bad file does not break sudo for anyone, but it prints
	// its syntax error on every sudo call by every user on the host for ever,
	// and Dokku shells through sudo constantly.
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the rule was installed although it does not parse: %v", statErr)
	}
	if _, statErr := os.Stat(staging); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the staging file was left in sudoers.d: %v", statErr)
	}
	if !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("the error does not carry what visudo said: %v", err)
	}
}

func TestTheSudoersFileIsOwnedByRootAndNotWritableByAnyoneElse(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "dokkup")
	staging := filepath.Join(dir, ".dokkup.tmp")

	tools, recorded := stubTools(t, nil)

	if err := tools.WriteSudoers(t.Context(), path, "dokkup ALL=(dokku) NOPASSWD: /usr/bin/dokku\n"); err != nil {
		t.Fatalf("WriteSudoers: %v", err)
	}

	// sudo rejects a sudoers.d file it does not like the look of and the rule
	// then grants nothing: measured, 0666 gives `sudo: /etc/sudoers.d/... is
	// world writable` and 0440 owned by anyone but root gives `is owned by uid
	// 996, should be 0`. The ownership is not asserted here because handing a
	// file to root needs root, and these tests do not have it; what the mode
	// asserts is that WriteFile's argument was not simply trusted through the
	// umask.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat of the installed rule: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o440 {
		t.Errorf("mode = %o, want 440", perm)
	}

	// visudo sees the dotfile, never the live name. Dotfiles in sudoers.d are
	// ignored by both sudo and `visudo -c`, so the rule is never half-visible
	// during the window, and the rename that ends it is atomic.
	want := []string{"visudo -cf " + staging}
	if got := recorded(); !slices.Equal(got, want) {
		t.Errorf("visudo was asked %v, want %v", got, want)
	}
	if _, statErr := os.Stat(staging); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the staging file outlived the rename: %v", statErr)
	}
}

func TestProvingTheSudoersRuleGoesThroughRunuserRatherThanSu(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		exit  int
		fails bool
	}{
		"the rule works": {exit: 0},
		"the rule does not let the service user in": {exit: 1, fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tools, recorded := stubTools(t, map[string]stub{host.Runuser: {exit: tc.exit}})

			err := tools.ProveSudo(t.Context(), "dokkup", "dokku", "/usr/bin/dokku", "version")
			if tc.fails && !errors.Is(err, host.ErrSudoRefused) {
				t.Fatalf("ProveSudo = %v, want %v", err, host.ErrSudoRefused)
			}
			if !tc.fails && err != nil {
				t.Fatalf("ProveSudo: %v", err)
			}

			// su is what must not appear. `su -s /bin/sh dokkup -c ...` starts
			// a `systemd --user` session that outlives the command -- measured,
			// pids 335371 and 335372 still there afterwards -- and that session
			// is what makes the matching uninstall's userdel fail with exit 8.
			// The runas is explicit so that the ceiling is the dokku account
			// rather than root.
			vectors := recorded()
			if len(vectors) != 1 {
				t.Fatalf("the host was asked %v, want one runuser invocation", vectors)
			}
			if !strings.HasPrefix(vectors[0], "runuser -u dokkup -- ") {
				t.Errorf("the proof did not go through runuser: %q", vectors[0])
			}
			if !strings.HasSuffix(vectors[0], " -n -u dokku /usr/bin/dokku version") {
				t.Errorf("the proof did not name the account Dokku is run as: %q", vectors[0])
			}
		})
	}
}

func TestADirectoryAnEarlierInstallationLeftWrongIsCorrectedRatherThanAccepted(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "dokkup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("standing in for a directory an earlier run left wrong: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("standing in for a directory an earlier run left wrong: %v", err)
	}

	tools, _ := stubTools(t, nil)
	owner := host.Account{Name: "dokkup", Uid: os.Getuid(), Gid: os.Getgid()}

	if err := tools.EnsureDir(t.Context(), dir, 0o750, owner); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}

	// MkdirAll alone would have left it: on a directory that exists it returns
	// nil and changes neither mode nor owner -- measured, a second MkdirAll
	// asking for 0700 over a 0750 directory left it 0750 and still owned by uid
	// 996. 0755 is exactly the wrong mode for a directory holding the database,
	// the audit trail and any private key.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat of the data directory: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o750 {
		t.Errorf("mode = %o, want 750: another unprivileged account can list it", perm)
	}
}

func TestACandidateVhostIsTestedWithoutTheLiveConfigurationBeingInvolved(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	candidate := filepath.Join(dir, "vhost.candidate")
	if err := os.WriteFile(candidate, []byte("server { listen 8443; }\n"), 0o644); err != nil {
		t.Fatalf("writing the candidate vhost: %v", err)
	}

	tools, recorded := stubTools(t, map[string]stub{host.Nginx: {dumps: true}})

	if err := tools.CheckProxyFragment(t.Context(), candidate); err != nil {
		t.Fatalf("CheckProxyFragment: %v", err)
	}

	// -c, so nothing under /etc/nginx is read at all. What this catches has to
	// be caught before the file goes in: nginx -t fails outright on an
	// ssl_certificate naming a file that is not there, and a failing file left
	// in conf.d does not break a reload, it breaks the next start.
	vectors := recorded()
	if len(vectors) == 0 || !strings.HasPrefix(vectors[0], "nginx -t -c ") {
		t.Fatalf("nginx was asked %v, want a -t -c over a wrapper", vectors)
	}
	if strings.Contains(vectors[0], "/etc/nginx") {
		t.Errorf("the isolated check reached into the live configuration: %q", vectors[0])
	}

	// The wrapper text, measured working against a real vhost on nginx 1.24:
	// events is mandatory or nginx refuses the file before reaching the
	// include, and the two logs are silenced so that a validation run cannot
	// write into the operator's logs.
	want := []string{
		"events { worker_connections 32; }",
		"http { access_log off; error_log /dev/null; include " + candidate + "; }",
	}
	if got := vectors[1:]; !slices.Equal(got, want) {
		t.Errorf("the wrapper was %v, want %v", got, want)
	}

	// Validation leaves nothing beside the candidate for the operator, or for
	// the next run, to trip over.
	if _, err := os.Stat(candidate + ".wrapper"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the wrapper outlived the check: %v", err)
	}
}

// Installation asks these before it creates anything, and "there is no such
// user" is the answer it acts on rather than one it reports. An ordinary error
// here would make a host that simply has no dokkup on it look broken.
func TestAnIdentityThisHostDoesNotHaveIsReportedAsAbsentRatherThanAsAFailure(t *testing.T) {
	t.Parallel()

	tools, _ := stubTools(t, nil)
	name := absentUser(t)

	if _, err := tools.LookupUser(t.Context(), name); !errors.Is(err, host.ErrNoSuchUser) {
		t.Errorf("LookupUser(%q) = %v, want %v", name, err, host.ErrNoSuchUser)
	}
	if _, err := tools.LookupGroup(t.Context(), name); !errors.Is(err, host.ErrNoSuchGroup) {
		t.Errorf("LookupGroup(%q) = %v, want %v", name, err, host.ErrNoSuchGroup)
	}
}

func TestAResolvedAccountCarriesTheNumbersChownNeeds(t *testing.T) {
	t.Parallel()

	current, err := user.Current()
	if err != nil {
		t.Fatalf("resolving the account these tests run as: %v", err)
	}
	wantUID, err := strconv.Atoi(current.Uid)
	if err != nil {
		t.Skipf("this host does not number its accounts: %v", err)
	}

	tools, _ := stubTools(t, nil)

	account, err := tools.LookupUser(t.Context(), current.Username)
	if err != nil {
		t.Fatalf("LookupUser: %v", err)
	}
	if account.Uid != wantUID {
		t.Errorf("uid = %d, want %d", account.Uid, wantUID)
	}
	if account.Name != current.Username {
		t.Errorf("name = %q, want %q", account.Name, current.Username)
	}
}
