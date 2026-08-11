package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/eduardotorresdev/dokkup/internal/authn"
	"github.com/eduardotorresdev/dokkup/internal/host"
	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/service"
)

func runUninstall(env Env, args []string) error {
	fs := newFlagSet(env, "uninstall")
	purge := fs.Bool("purge", false, "delete the data directory without asking")

	if err := parseFlags(fs, args); err != nil {
		return err
	}

	// The report is printed every time, as part of the command rather than
	// behind a flag. Someone removing dokkup should not have to have known to
	// ask what it is about to do.
	PrintRemovalReport(env, *purge)

	if err := uninstallPreflight(); err != nil {
		return err
	}

	confirmer := &authn.Confirmer{
		In:       env.Stdin,
		Out:      env.Stdout,
		Password: authn.SudoPassword(),
	}
	un := &uninstaller{
		env:     env,
		ops:     host.NewTools(),
		manager: service.NewSystemctl(),
		confirm: confirmer.Confirm,
		purge:   *purge,
	}

	return un.uninstall(context.Background())
}

func uninstallPreflight() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("dokkup is installed on Linux hosts; this is %s", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		return errors.New("run as root: this removes a system user, a sudoers rule and a " +
			"system service")
	}
	return nil
}

// uninstaller performs one removal. As with [installer], everything it touches
// the outside world through is a field, so a test can watch it remove a host it
// invented without being root on a real one.
type uninstaller struct {
	env     Env
	ops     host.Ops
	manager service.Manager

	// confirm proves the operator is present and meant this.
	confirm func(ctx context.Context) error

	// root is prefixed to every path, as in [installer].
	root string

	purge bool

	// removeData is what was decided about the data directory, before anything
	// was removed and while the answer could still change what happens.
	removeData bool

	removed []string
	kept    []string

	// failed is what could not be removed. Removal does not stop at the first
	// one: a host is better off with six of seven things gone and a list of
	// what is left than with one thing gone and an error.
	failed []string
}

func (u *uninstaller) path(p string) string { return hostpaths.Rooted(u.root, p) }

func (u *uninstaller) uninstall(ctx context.Context) error {
	if err := u.decideAboutData(); err != nil {
		return err
	}

	if err := u.confirm(ctx); err != nil {
		return err
	}
	printf(u.env.Stdout, "\n")

	// The proxy goes first, so that nginx stops sending requests to a service
	// that is about to stop answering them, rather than turning the last
	// moments of the removal into 502s for whoever is watching.
	u.removeVhost(ctx)

	// Before the user, and not optional. userdel refuses with exit 8 while a
	// process is still running as the account -- measured on the devenv against
	// a live unit: `userdel: user zz-probe-svc is currently used by process
	// 342857`. Answering that with --force is not the fix.
	//
	// Asked for only when there is a unit to disable. `systemctl disable --now`
	// on a unit whose file is already gone exits 1 -- measured on the devenv:
	// `Failed to disable unit: Unit file dokkup.service does not exist.` -- and
	// removal has to be re-runnable, so a second run finding nothing must not
	// report this host as incompletely removed. It is the same rule
	// [uninstaller.removeFile] already keeps for a file that is not there.
	if exists(u.path(hostpaths.Unit)) {
		if err := u.manager.Disable(ctx); err != nil {
			u.failf("stopping and disabling %s: %v", service.Name, err)
		} else {
			u.removed = append(u.removed,
				"the running service, stopped and no longer started at boot")
		}
	}

	u.removeUnit(ctx)
	u.removeFile(hostpaths.Sudoers)
	u.removeFile(hostpaths.Binary)
	u.removeFile(hostpaths.PreviousBinary)

	// The certificate goes whether or not the data directory stays. It is not
	// operator data, and an uninstall that keeps a database has no reason to
	// keep a private key.
	u.removeTree(hostpaths.TLSDir)

	u.removeAccount(ctx)
	u.removeDataDir()

	u.report()

	if len(u.failed) > 0 {
		return fmt.Errorf("dokkup is not completely removed from this host: %s",
			strings.Join(u.failed, "; "))
	}
	return nil
}

// decideAboutData settles the one irreversible question before the operator is
// asked to confirm anything, so that what they confirm is the whole of what
// will happen.
//
// It is also asked before [authn.Confirmer] reads anything, because that is the
// order the two can share one standard input: this reads a line and not a byte
// more, so the answer to the challenge is still there afterwards. `printf
// 'no\ndokkup-host\n' | dokkup uninstall` works.
func (u *uninstaller) decideAboutData() error {
	if u.purge {
		u.removeData = true
		return nil
	}
	if !exists(u.path(hostpaths.DataDir)) {
		// Nothing to ask about. A host that never had one, or a second run.
		return nil
	}

	yes, err := yesOrNo(u.env, fmt.Sprintf(
		"%s holds the operators, the audit trail and the deploy history. Nothing\n"+
			"else on this host reads it, and removing dokkup does not need it gone.",
		hostpaths.DataDir),
		"Remove it as well?")
	switch {
	case errors.Is(err, errNoAnswer):
		return fmt.Errorf("there is no answer to read, and removal will not decide what happens "+
			"to %s for you: pass --purge to remove it, and nothing has been removed",
			hostpaths.DataDir)
	case err != nil:
		return err
	}

	u.removeData = yes
	return nil
}

func (u *uninstaller) removeVhost(ctx context.Context) {
	path := u.path(hostpaths.Vhost)
	if !exists(path) {
		return
	}
	if err := os.Remove(path); err != nil {
		u.failf("removing %s: %v", hostpaths.Vhost, err)
		return
	}
	u.removed = append(u.removed, hostpaths.Vhost)

	// Tested before the reload, and the reload skipped rather than forced if
	// the rest of this host's configuration does not pass. dokkup's file is
	// already gone, so nginx picks that up at its next start whatever happens
	// here, and reloading onto a configuration that fails would turn somebody
	// else's broken vhost into an outage dokkup caused.
	if err := u.ops.CheckProxy(ctx); err != nil {
		u.warnf("%s is removed, but nginx does not accept the rest of this host's "+
			"configuration, so it was not reloaded: %v", hostpaths.Vhost, err)
		return
	}
	if err := u.manager.Reload(ctx, nginxUnit); err != nil {
		u.warnf("asking nginx to forget %s: %v", hostpaths.Vhost, err)
	}
}

func (u *uninstaller) removeUnit(ctx context.Context) {
	if !u.removeFile(hostpaths.Unit) {
		return
	}
	// systemd is still holding the text of a unit whose file is gone, and would
	// go on reporting it until something told it otherwise.
	if err := u.manager.DaemonReload(ctx); err != nil {
		u.warnf("telling systemd that %s is gone: %v", hostpaths.Unit, err)
	}
}

// removeFile removes one of dokkup's files and reports whether there was one.
// A file that is already gone is not a failure: removal has to be re-runnable,
// because the reason someone runs it twice is usually that the first run
// stopped part way.
func (u *uninstaller) removeFile(path string) bool {
	if err := os.Remove(u.path(path)); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			u.failf("removing %s: %v", path, err)
		}
		return false
	}
	u.removed = append(u.removed, path)
	return true
}

func (u *uninstaller) removeTree(path string) {
	full := u.path(path)
	if !exists(full) {
		return
	}
	if err := os.RemoveAll(full); err != nil {
		u.failf("removing %s: %v", path, err)
		return
	}
	u.removed = append(u.removed, path)
}

// removeAccount removes dokkup's own identity and nothing else's.
//
// The dokku group is never touched. userdel strips dokkup's membership of it
// and leaves the group itself intact -- measured -- so there is nothing here to
// do, and doing anything would be removing something dokkup did not create.
func (u *uninstaller) removeAccount(ctx context.Context) {
	removed, err := u.ops.RemoveUser(ctx, hostpaths.User)
	switch {
	case errors.Is(err, host.ErrUserBusy):
		u.failf("the user %s could not be removed because something is still running as it; "+
			"stop it and run this again. dokkup will not force it: forcing succeeds while "+
			"leaving processes under a uid this host is free to hand out again",
			hostpaths.User)
		return
	case err != nil:
		u.failf("removing the user %s: %v", hostpaths.User, err)
		return
	case removed:
		u.removed = append(u.removed, "the system user "+hostpaths.User)
	}

	// userdel takes a same-named primary group with it when nothing else
	// belongs to it, so "there was none" is the ordinary answer here rather
	// than a surprise. What is reported is what was found, because a removal
	// that names a group it did not take reads as work done on a host where
	// nothing was left to do.
	tookGroup, err := u.ops.RemoveGroup(ctx, hostpaths.Group)
	if err != nil {
		u.failf("removing the group %s: %v", hostpaths.Group, err)
		return
	}
	if tookGroup {
		u.removed = append(u.removed, "the system group "+hostpaths.Group)
	}
}

func (u *uninstaller) removeDataDir() {
	path := u.path(hostpaths.DataDir)
	if !exists(path) {
		return
	}
	if !u.removeData {
		u.kept = append(u.kept, hostpaths.DataDir+" -- operators, audit trail, deploy history")
		return
	}
	if err := os.RemoveAll(path); err != nil {
		u.failf("removing %s: %v", hostpaths.DataDir, err)
		return
	}
	u.removed = append(u.removed, hostpaths.DataDir)
}

func (u *uninstaller) failf(format string, a ...any) {
	message := fmt.Sprintf(format, a...)
	u.failed = append(u.failed, message)
	printf(u.env.Stderr, "  %s\n", message)
}

// warnf reports something that went wrong and did not stop the removal, and
// does not make it a failure. Nginx not reloading is the example: dokkup's file
// is gone either way.
func (u *uninstaller) warnf(format string, a ...any) {
	printf(u.env.Stderr, "  "+format+"\n", a...)
}

func (u *uninstaller) report() {
	if len(u.removed) == 0 {
		printf(u.env.Stdout, "There was nothing of dokkup's left on this host.\n\n")
	} else {
		printf(u.env.Stdout, "\nRemoved:\n\n")
		for _, what := range u.removed {
			printf(u.env.Stdout, "    %s\n", what)
		}
		printf(u.env.Stdout, "\n")
	}

	if len(u.kept) > 0 {
		printf(u.env.Stdout, "  Left in place:\n\n")
		for _, what := range u.kept {
			printf(u.env.Stdout, "    %s\n", what)
		}
		printf(u.env.Stdout, "\n")
	}

	printf(u.env.Stdout, "  Your apps, your volumes, this host's Dokku plugins and Dokku itself\n")
	printf(u.env.Stdout, "  are as they were.\n\n")
}

// removalTargets is every path removal takes.
//
// It is built from hostpaths rather than listed here, so that a location added
// to what dokkup puts on a host cannot be added to installation and forgotten
// by removal. The data directory is not in it because it is asked about rather
// than removed.
func removalTargets() []string {
	targets := make([]string, 0, len(hostpaths.Owned())+len(hostpaths.Conditional()))
	for _, path := range hostpaths.Owned() {
		if path != hostpaths.DataDir {
			targets = append(targets, path)
		}
	}
	return append(targets, hostpaths.Conditional()...)
}

// removalNotes says why a path is in the list, where that is not obvious from
// the path itself.
var removalNotes = map[string]string{
	hostpaths.PreviousBinary: "kept by an update or a reinstall, if there was one",
	hostpaths.Vhost:          "dokkup's one file under /etc/nginx",
	hostpaths.TLSDir:         "the certificate dokkup serves, if it has one",
}

// PrintRemovalReport states exactly what removal takes and what it leaves.
//
// The second list matters more than the first. Uninstalling a management
// interface must not break the server it was managing, and saying so plainly is
// how an operator can trust that it will not.
func PrintRemovalReport(env Env, purge bool) {
	printf(env.Stdout, "dokkup uninstall will REMOVE:\n\n")
	for _, path := range removalTargets() {
		if note, ok := removalNotes[path]; ok {
			printf(env.Stdout, "    %s  (%s)\n", path, note)
			continue
		}
		printf(env.Stdout, "    %s\n", path)
	}
	printf(env.Stdout, "    system user and group %q\n", hostpaths.User)

	if purge {
		printf(env.Stdout, "    %s  (--purge given: operators, audit trail and deploy history)\n",
			hostpaths.DataDir)
	} else {
		printf(env.Stdout, "\n  and will ASK before removing:\n\n")
		printf(env.Stdout, "    %s  (operators, audit trail, deploy history)\n", hostpaths.DataDir)
	}

	printf(env.Stdout, "\n  It will NOT touch:\n\n")
	printf(env.Stdout, "    your apps, and nothing they run\n")
	printf(env.Stdout, "    volumes under %s -- your data stays\n", hostpaths.DokkuStorageRoot)
	printf(env.Stdout, "    networks connecting your apps\n")
	printf(env.Stdout, "    Dokku plugins, including a Let's Encrypt plugin if this host has\n")
	printf(env.Stdout, "      one: removing it would stop certificate renewal for apps that\n")
	printf(env.Stdout, "      have nothing to do with dokkup\n")
	printf(env.Stdout, "    registry credentials held by Docker\n")
	printf(env.Stdout, "    Dokku itself\n\n")

	printf(env.Stdout, "  You will be asked to authenticate again before anything is removed:\n")
	printf(env.Stdout, "  your password, or the host's name if you are already root.\n\n")
}
