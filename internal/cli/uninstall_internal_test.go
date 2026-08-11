package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/authn"
	"github.com/eduardotorresdev/dokkup/internal/dokku"
	"github.com/eduardotorresdev/dokkup/internal/host"
	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/service"
)

// testHostname is the name the challenge asks for. It is given to the
// [authn.Confirmer] explicitly rather than read from os.Hostname, so that a
// test asserting a wrong answer cannot accidentally type the right one on
// somebody's laptop.
const testHostname = "test-host"

// dataFile stands for what an operator would lose. Removal asks about the
// directory rather than about the database, so the question is only worth
// answering if there is something inside to keep.
const dataFile = hostpaths.DataDir + "/dokkup.db"

// whatInstallationLeft is every file a finished installation leaves on a host.
// The contents name each one, so that an assertion can tell a file that
// survived from one that was put back.
func whatInstallationLeft() map[string]string {
	return map[string]string{
		hostpaths.Binary:         "the dokkup binary",
		hostpaths.PreviousBinary: "the binary an update replaced",
		hostpaths.Unit:           "[Service]\nExecStart=" + hostpaths.Binary + " serve\n",
		hostpaths.Sudoers: fmt.Sprintf("%s ALL=(%s) NOPASSWD: %s\n",
			hostpaths.User, dokku.DefaultRunAs, dokku.DefaultBinary),
		hostpaths.Vhost:   "server { listen 8443 ssl; }\n",
		hostpaths.TLSCert: "a certificate",
		hostpaths.TLSKey:  "a private key",
		dataFile:          "the operators, the audit trail and the deploy history",
	}
}

// callLog is one ordered record of what was asked of both seams.
//
// The property this file leans on hardest -- that the service stops before the
// account it runs as is removed -- spans host.Ops and service.Manager, and
// neither fake can see the other's calls. Nothing is stubbed to get it: the two
// wrappers below note the call and hand it straight to the fake underneath.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) note(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.calls = append(l.calls, call)
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Clone(l.calls)
}

type removalOps struct {
	host.Ops
	log *callLog
}

func (o removalOps) RemoveUser(ctx context.Context, name string) (bool, error) {
	o.log.note("remove-user " + name)
	return o.Ops.RemoveUser(ctx, name)
}

func (o removalOps) CheckProxy(ctx context.Context) error {
	o.log.note("check-proxy")
	return o.Ops.CheckProxy(ctx)
}

type removalManager struct {
	service.Manager
	log *callLog
}

func (m removalManager) Disable(ctx context.Context) error {
	m.log.note("disable")
	return m.Manager.Disable(ctx)
}

func (m removalManager) Reload(ctx context.Context, unit string) error {
	m.log.note("reload " + unit)
	return m.Manager.Reload(ctx, unit)
}

// removal is a host with dokkup on it and the uninstaller that is about to take
// it off again. Everything the uninstaller reaches the outside world through is
// a field of it, so none of this needs root, systemd, nginx or a Dokku host.
type removal struct {
	t       *testing.T
	root    string
	ops     *host.Fake
	manager *service.Fake
	log     *callLog
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	un      *uninstaller
}

// newInstalledHost puts files under a temporary root and gives the host the
// identities installation creates, with the unit running.
func newInstalledHost(t *testing.T, files map[string]string, stdin io.Reader) *removal {
	t.Helper()

	r := &removal{
		t:       t,
		root:    t.TempDir(),
		ops:     host.NewFake(),
		manager: &service.Fake{Active: true},
		log:     &callLog{},
		stdout:  &bytes.Buffer{},
		stderr:  &bytes.Buffer{},
	}
	for path, content := range files {
		r.write(path, content)
	}

	// The identity side of an installed host. The fake arrives with Dokku's own
	// user and group; these are what installation added beside them. The
	// membership is not decoration: it is what makes "the dokku group survived
	// with its members" a question worth asking of a removal.
	r.ops.Groups[hostpaths.Group] = host.Account{Name: hostpaths.Group, Uid: -1, Gid: 999}
	r.ops.Users[hostpaths.User] = host.Account{Name: hostpaths.User, Uid: 999, Gid: 999}
	r.ops.Memberships[hostpaths.User] = []string{hostpaths.DokkuGroup}

	r.un = &uninstaller{
		env:     Env{Stdin: stdin, Stdout: r.stdout, Stderr: r.stderr},
		ops:     removalOps{Ops: r.ops, log: r.log},
		manager: removalManager{Manager: r.manager, log: r.log},
		confirm: func(context.Context) error { return nil },
		root:    r.root,
	}
	return r
}

func (r *removal) write(path, content string) {
	r.t.Helper()

	full := hostpaths.Rooted(r.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatalf("making the directory for %s: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatalf("writing %s: %v", path, err)
	}
}

// read reports what is at path, and the empty string when there is nothing
// there, so an assertion can say what it wanted and what it got in one line.
func (r *removal) read(path string) string {
	r.t.Helper()

	content, err := os.ReadFile(hostpaths.Rooted(r.root, path))
	if err != nil {
		return ""
	}
	return string(content)
}

func (r *removal) present(path string) bool {
	r.t.Helper()

	_, err := os.Stat(hostpaths.Rooted(r.root, path))
	return err == nil
}

func (r *removal) uninstall() error {
	r.t.Helper()

	return r.un.uninstall(r.t.Context())
}

// mustPrecede fails unless both calls were made, in this order.
func (r *removal) mustPrecede(first, second string) {
	r.t.Helper()

	calls := r.log.all()
	at, then := slices.Index(calls, first), slices.Index(calls, second)
	switch {
	case at < 0:
		r.t.Errorf("%q was never asked for at all: %v", first, calls)
	case then < 0:
		r.t.Errorf("%q was never asked for at all: %v", second, calls)
	case at > then:
		r.t.Errorf("%q came after %q, and has to come before it: %v", first, second, calls)
	}
}

// newRefusedRemoval is an installed host whose confirmation is the real one,
// reading whatever the operator typed.
//
// --purge is given because it is the graver command: what a refusal has to hold
// back is a removal that was told to take the database with it.
func newRefusedRemoval(t *testing.T, typed string) *removal {
	t.Helper()

	r := newInstalledHost(t, whatInstallationLeft(), strings.NewReader(typed))
	r.un.purge = true

	// The real Confirmer rather than a closure, because what is being asserted
	// here is the wiring between the command and internal/authn. A closure that
	// returned an error would assert nothing about it.
	confirmer := &authn.Confirmer{In: r.un.env.Stdin, Out: r.stdout, Hostname: testHostname}
	r.un.confirm = confirmer.Confirm

	return r
}

// assertNothingWasRemoved is what a refusal has to leave behind: every file,
// every identity, and a service still running.
func (r *removal) assertNothingWasRemoved(err error) {
	r.t.Helper()

	if !errors.Is(err, authn.ErrNotConfirmed) {
		r.t.Fatalf("uninstall = %v, want it to wrap authn.ErrNotConfirmed", err)
	}

	for path, want := range whatInstallationLeft() {
		if got := r.read(path); got != want {
			r.t.Errorf("%s = %q, want %q: an unconfirmed removal took it anyway", path, got, want)
		}
	}
	for _, call := range r.ops.Calls {
		if strings.HasPrefix(call, "remove-") {
			r.t.Errorf("an unconfirmed removal asked the host for %q: %v", call, r.ops.Calls)
		}
	}
	if _, ok := r.ops.Users[hostpaths.User]; !ok {
		r.t.Errorf("the system user %s is gone although nobody confirmed anything", hostpaths.User)
	}
	if slices.Contains(r.manager.Calls, "disable") || !r.manager.Active {
		r.t.Errorf("the service was stopped although nobody confirmed anything: %v", r.manager.Calls)
	}
}

// The order is measured, and it is not tidiness: userdel refuses with exit 8
// while a process is still running as the account -- on the devenv, against a
// live unit, `userdel: user zz-probe-svc is currently used by process 342857`,
// recorded at the step in uninstall.go this pins. An uninstaller that took the
// account first would meet that refusal with nothing left to answer it but
// --force, which is the one answer it must never give.
func TestUninstallStopsTheServiceBeforeRemovingTheUserItRunsAs(t *testing.T) {
	t.Parallel()

	r := newInstalledHost(t, whatInstallationLeft(), strings.NewReader(""))
	r.un.purge = true

	if err := r.uninstall(); err != nil {
		t.Fatalf("uninstall = %v, want nil", err)
	}

	r.mustPrecede("disable", "remove-user "+hostpaths.User)

	if r.manager.Active {
		t.Error("the service is still running, so the account it runs as could not have gone")
	}
	if _, ok := r.ops.Users[hostpaths.User]; ok {
		t.Errorf("the system user %s is still on this host", hostpaths.User)
	}
}

// The other order would turn the last moments of a removal into 502s for
// whoever is watching it happen: nginx would go on sending requests to a
// service that had already stopped answering them.
func TestTheProxyForgetsDokkupBeforeTheServiceStopsAnswering(t *testing.T) {
	t.Parallel()

	r := newInstalledHost(t, whatInstallationLeft(), strings.NewReader(""))
	r.un.purge = true

	if err := r.uninstall(); err != nil {
		t.Fatalf("uninstall = %v, want nil", err)
	}

	if r.present(hostpaths.Vhost) {
		t.Errorf("%s is still under /etc/nginx", hostpaths.Vhost)
	}
	r.mustPrecede("reload "+nginxUnit, "disable")

	// Tested before it is reloaded, never after: a reload is what makes a
	// configuration live, and asking for one without knowing it passes is how a
	// removal breaks a host it was only meant to leave.
	r.mustPrecede("check-proxy", "reload "+nginxUnit)
}

// A configuration this host was already failing is somebody else's problem, and
// reloading onto it would make it dokkup's. The file is gone either way, so
// nginx picks that up at its next start whatever happens here.
func TestNginxIsNotReloadedOntoAConfigurationItAlreadyRefuses(t *testing.T) {
	t.Parallel()

	r := newInstalledHost(t, whatInstallationLeft(), strings.NewReader(""))
	r.un.purge = true
	r.ops.FailAt = "check-proxy"

	if err := r.uninstall(); err != nil {
		t.Fatalf("uninstall = %v, want nil: another vhost failing is not a failed removal", err)
	}

	if r.present(hostpaths.Vhost) {
		t.Errorf("%s survived, and it is the whole of dokkup's presence in the proxy",
			hostpaths.Vhost)
	}
	if slices.Contains(r.manager.Calls, "reload "+nginxUnit) {
		t.Errorf("nginx was reloaded onto a configuration it refuses: %v", r.manager.Calls)
	}
	if !strings.Contains(r.stderr.String(), "not reloaded") {
		t.Errorf("the operator was not told the reload was skipped: %q", r.stderr.String())
	}
}

func TestUninstallRemovesTheCertificateEvenWhenTheDataDirectoryStays(t *testing.T) {
	t.Parallel()

	r := newInstalledHost(t, whatInstallationLeft(), strings.NewReader("no\n"))

	if err := r.uninstall(); err != nil {
		t.Fatalf("uninstall = %v, want nil", err)
	}

	if got, want := r.read(dataFile), whatInstallationLeft()[dataFile]; got != want {
		t.Errorf("%s = %q, want %q: the answer was no", dataFile, got, want)
	}
	if !strings.Contains(r.stdout.String(), hostpaths.DataDir) ||
		!strings.Contains(r.stdout.String(), "Left in place") {
		t.Errorf("the operator was not told what was kept: %q", r.stdout.String())
	}

	// A certificate is not operator data, and an uninstall that keeps the
	// database has no reason to keep a private key.
	for _, path := range []string{hostpaths.TLSKey, hostpaths.TLSCert, hostpaths.TLSDir} {
		if r.present(path) {
			t.Errorf("%s survived the removal of everything else", path)
		}
	}
}

// The dokku group is Dokku's, not dokkup's. Removing a group dokkup did not
// create would be removing something else's identity, and userdel has already
// stripped dokkup's membership of it by the time this is over.
func TestUninstallNeverAsksToRemoveTheDokkuGroup(t *testing.T) {
	t.Parallel()

	r := newInstalledHost(t, whatInstallationLeft(), strings.NewReader(""))
	r.un.purge = true

	if err := r.uninstall(); err != nil {
		t.Fatalf("uninstall = %v, want nil", err)
	}

	// Compared whole rather than by substring, because "remove-group dokkup"
	// begins with "remove-group dokku".
	for _, call := range r.ops.Calls {
		if call == "remove-group "+hostpaths.DokkuGroup {
			t.Errorf("removal asked to remove the %q group: %v", hostpaths.DokkuGroup, r.ops.Calls)
		}
	}
	if _, ok := r.ops.Groups[hostpaths.DokkuGroup]; !ok {
		t.Errorf("the %q group is gone, and every app on this host is deployed through it",
			hostpaths.DokkuGroup)
	}
	if _, ok := r.ops.Users[dokku.DefaultRunAs]; !ok {
		t.Errorf("the %q account is gone, and Dokku itself runs as it", dokku.DefaultRunAs)
	}
}

// The drift guard. Every location dokkup puts on a host is named in exactly one
// place, and this reads that place rather than restating it: a path added to
// installation and forgotten by removal arrives here on its own and fails until
// removal visits it too.
func TestEverythingHostpathsKnowsAboutIsEitherRemovedOrAskedAbout(t *testing.T) {
	t.Parallel()

	known := append(hostpaths.Owned(), hostpaths.Conditional()...)

	for name, tc := range map[string]struct {
		purge    bool
		typed    string
		survives []string
	}{
		"with --purge, every one of them is taken": {purge: true},

		// The data directory is the one path removal asks about rather than
		// takes, and what is inside it has to survive the answer whole.
		"without it, the data directory is asked about and kept": {
			typed:    "no\n",
			survives: []string{hostpaths.DataDir, hostpaths.DataDir + keptMarker},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := newInstalledHost(t, knownPaths(known), strings.NewReader(tc.typed))
			r.un.purge = tc.purge

			if err := r.uninstall(); err != nil {
				t.Fatalf("uninstall = %v, want nil", err)
			}

			for _, path := range known {
				if slices.Contains(tc.survives, path) {
					continue
				}
				if r.present(path) {
					t.Errorf("%s is still on this host: hostpaths names it and removal never "+
						"visited it", path)
				}
			}
			for _, path := range tc.survives {
				if !r.present(path) {
					t.Errorf("%s was taken although removal asks about it rather than taking it",
						path)
				}
			}
		})
	}
}

// keptMarker is the file put inside a location that other locations live under,
// so that a directory removal is meant to keep can be seen keeping its contents
// rather than merely existing.
const keptMarker = "/put-here-by-an-installation"

// knownPaths populates every location hostpaths names: a file, unless another
// known path lives underneath it, in which case a file inside it.
//
// That rule is as much as this can work out from the list alone, and it is
// enough for what the guard watches for, which is a location removal never
// visits at all.
func knownPaths(known []string) map[string]string {
	files := make(map[string]string, len(known))
	for _, path := range known {
		at := path
		for _, other := range known {
			if strings.HasPrefix(other, path+"/") {
				at = path + keptMarker
				break
			}
		}
		files[at] = "put here by an installation"
	}
	return files
}

func TestAWrongAnswerRemovesNothingAtAll(t *testing.T) {
	t.Parallel()

	r := newRefusedRemoval(t, "not-the-host\n")

	r.assertNothingWasRemoved(r.uninstall())
}

// `dokkup uninstall < /dev/null`. A closed standard input has said nothing, and
// nothing is not an answer -- least of all to the one question that stands
// between a mis-typed command and an irreversible removal.
func TestNoAnswerAtAllRemovesNothingEither(t *testing.T) {
	t.Parallel()

	r := newRefusedRemoval(t, "")

	r.assertNothingWasRemoved(r.uninstall())
}

// What the unbuffered readLine in install.go exists for.
//
// A bufio.Reader answering the data-directory question would have taken the
// answer to the challenge out of the pipe with it and left the challenge
// reading nothing, so `printf 'no\ntest-host\n' | dokkup uninstall` would have
// refused an operator who answered both questions correctly.
func TestOneStandardInputCarriesBothAnswers(t *testing.T) {
	t.Parallel()

	r := newInstalledHost(t, whatInstallationLeft(), strings.NewReader("no\n"+testHostname+"\n"))
	confirmer := &authn.Confirmer{In: r.un.env.Stdin, Out: r.stdout, Hostname: testHostname}
	r.un.confirm = confirmer.Confirm

	if err := r.uninstall(); err != nil {
		t.Fatalf("uninstall = %v, want nil: both answers were on the one standard input", err)
	}

	if got, want := r.read(dataFile), whatInstallationLeft()[dataFile]; got != want {
		t.Errorf("%s = %q, want %q: the first answer was no", dataFile, got, want)
	}
	for _, path := range []string{
		hostpaths.Binary, hostpaths.Unit, hostpaths.Sudoers, hostpaths.Vhost, hostpaths.TLSDir,
	} {
		if r.present(path) {
			t.Errorf("%s survived, so the second answer never reached the challenge", path)
		}
	}
	if _, ok := r.ops.Users[hostpaths.User]; ok {
		t.Errorf("the system user %s survived, so the removal stopped at the challenge",
			hostpaths.User)
	}
}

// The reason somebody runs removal twice is usually that the first run stopped
// part way, so the second must be able to finish the job rather than refusing
// to start it.
func TestRemovingTwiceIsNotAnError(t *testing.T) {
	t.Parallel()

	r := newInstalledHost(t, whatInstallationLeft(), strings.NewReader(""))
	r.un.purge = true

	if err := r.uninstall(); err != nil {
		t.Fatalf("the first uninstall = %v, want nil", err)
	}

	// The same host and the same root, with nothing of dokkup's left in either.
	var stdout, stderr bytes.Buffer
	second := &uninstaller{
		env:     Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr},
		ops:     r.un.ops,
		manager: r.un.manager,
		confirm: func(context.Context) error { return nil },
		root:    r.root,
		purge:   true,
	}

	if err := second.uninstall(t.Context()); err != nil {
		t.Fatalf("the second uninstall = %v, want nil", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("the second removal complained about something already gone: %q", stderr.String())
	}
	for _, path := range removalTargets() {
		if slices.Contains(second.removed, path) {
			t.Errorf("the second removal reported taking %s, and there was nothing there", path)
		}
		if r.present(path) {
			t.Errorf("%s came back between the two runs", path)
		}
	}
}

// The report the second run prints, which is the operator's only account of
// what just happened to their host.
func TestASecondRemovalSaysThereWasNothingLeftRatherThanNamingWhatItDidNotTake(t *testing.T) {
	t.Parallel()

	r := newInstalledHost(t, whatInstallationLeft(), strings.NewReader(""))
	r.un.purge = true

	if err := r.uninstall(); err != nil {
		t.Fatalf("the first uninstall = %v, want nil", err)
	}

	var stdout bytes.Buffer
	second := &uninstaller{
		env:     Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &bytes.Buffer{}},
		ops:     r.un.ops,
		manager: r.un.manager,
		confirm: func(context.Context) error { return nil },
		root:    r.root,
		purge:   true,
	}
	if err := second.uninstall(t.Context()); err != nil {
		t.Fatalf("the second uninstall = %v, want nil", err)
	}

	if !strings.Contains(stdout.String(), "There was nothing of dokkup's left on this host.") {
		t.Errorf("a host with nothing of dokkup's on it was told what was removed from it: %q",
			stdout.String())
	}
}

// Forcing it succeeds while leaving processes running under a uid this host is
// then free to hand out again, which is how a removal ends with somebody else's
// service owning dokkup's old files. The operator is told what to stop instead.
func TestSomethingStillRunningAsTheUserIsReportedRatherThanForced(t *testing.T) {
	t.Parallel()

	r := newInstalledHost(t, whatInstallationLeft(), strings.NewReader(""))
	r.un.purge = true
	r.un.ops = busyOps{Ops: r.un.ops}

	err := r.uninstall()
	if err == nil {
		t.Fatal("a removal that could not take the account it was told to take reported success")
	}
	if !strings.Contains(err.Error(), "stop it and run this again") {
		t.Errorf("error = %v, want it to say what the operator can do about it", err)
	}

	everywhere := append(slices.Clone(r.ops.Calls), r.stdout.String(), r.stderr.String(), err.Error())
	for _, said := range everywhere {
		if strings.Contains(said, "--force") {
			t.Errorf("--force reached the host, or was offered to the operator: %q", said)
		}
	}

	// The account is left whole rather than half taken. Its group is what its
	// files are owned by, and removing that while the account stays would leave
	// a host in a state neither the operator nor a second run can reason about.
	if _, ok := r.ops.Users[hostpaths.User]; !ok {
		t.Errorf("the system user %s went after all, so this test proved nothing", hostpaths.User)
	}
	if _, ok := r.ops.Groups[hostpaths.Group]; !ok {
		t.Errorf("the group %s was taken from an account that is still there", hostpaths.Group)
	}

	// Reported, and not stopped at: what came before the account had already
	// gone, and what comes after still goes.
	for _, path := range []string{hostpaths.Binary, hostpaths.Unit, hostpaths.DataDir} {
		if r.present(path) {
			t.Errorf("%s survived a failure that had nothing to do with it", path)
		}
	}
}

// busyOps is a host where something is still running as the service account.
// That is userdel's exit 8, and the measurement behind it is recorded at the
// step in uninstall.go this exercises.
type busyOps struct {
	host.Ops
}

func (b busyOps) RemoveUser(context.Context, string) (bool, error) {
	return false, fmt.Errorf("%w: process 342857", host.ErrUserBusy)
}

// A host is better off with six of seven things gone and a list of what is
// left than with one thing gone and an error.
func TestAFailureToRemoveOneThingDoesNotStopTheRest(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root is not stopped by the directory mode this makes the one failure with")
	}

	r := newInstalledHost(t, whatInstallationLeft(), strings.NewReader(""))
	r.un.purge = true

	// Removing a file needs write permission on the directory holding it rather
	// than on the file, so taking that away fails exactly one removal and
	// leaves every other one alone.
	dir := filepath.Dir(hostpaths.Rooted(r.root, hostpaths.Sudoers))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("making %s unwritable: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := r.uninstall()
	if err == nil {
		t.Fatal("a removal that left the sudoers rule behind reported success")
	}
	if !strings.Contains(err.Error(), hostpaths.Sudoers) {
		t.Errorf("error = %v, want it to name what is still on this host", err)
	}
	// The framing is the whole point of going on after a failure: an operator
	// who is told only that one file could not be removed does not know whether
	// the other six went, and has no way to find out but by looking.
	if !strings.Contains(err.Error(), "not completely removed") {
		t.Errorf("error = %v, want it to say that this host is partly cleared rather than "+
			"reporting the one failure on its own", err)
	}
	if !r.present(hostpaths.Sudoers) {
		t.Errorf("%s went after all, so this test proved nothing", hostpaths.Sudoers)
	}

	for _, path := range []string{
		hostpaths.Unit, hostpaths.Binary, hostpaths.PreviousBinary,
		hostpaths.Vhost, hostpaths.TLSDir, hostpaths.DataDir,
	} {
		if r.present(path) {
			t.Errorf("%s survived a failure that had nothing to do with it", path)
		}
	}
	if _, ok := r.ops.Users[hostpaths.User]; ok {
		t.Errorf("the system user %s survived a failure that had nothing to do with it",
			hostpaths.User)
	}
}
