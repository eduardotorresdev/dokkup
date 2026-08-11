//go:build integration

package cli_test

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/dokku"
	"github.com/eduardotorresdev/dokkup/internal/host"
	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/proxy"
	"github.com/eduardotorresdev/dokkup/internal/service"
)

// This is `dokkup install` and `dokkup uninstall` against a real Dokku host,
// with real shadow-utils, real sudo, real visudo, real nginx and real systemd,
// on a host that has an application on it.
//
// The unit tests prove the decisions against fakes. What a fake cannot be
// convincingly wrong about is any of this: that useradd, usermod, userdel and
// groupdel exit the way the table in internal/host says they do; that the
// sudoers rule dokkup writes really lets the service run Dokku and really lets
// it run nothing else; that nginx accepts the vhost and serves the certificate
// whose fingerprint the operator was told to compare in a browser; and that
// when all of it is removed the host is the host it was.
//
// Nothing here runs in parallel, and that is deliberate rather than an
// oversight. There is one dokkup-devenv, there is one /usr/local/bin/dokkup on
// it, and the subtests are one installation told in order: each of them reads
// the host the one before it left.

const (
	// installVersion is what the binary under test reports. No other test in
	// this package publishes it, so a service answering as it can only be the
	// one this test installed.
	installVersion = "v0.12.0"

	// installIP is the address dokkup is installed at. Loopback rather than
	// this host's own, because what is being proved is that nginx serves dokkup
	// at all, and loopback is the one address the container has under both
	// runtimes.
	installIP = "127.0.0.1"

	// absent is what [devenv.ownedPaths] answers for a path that is not there,
	// so that "gone" and "750:dokkup:dokkup" are the same kind of answer and a
	// removal and an installation can be checked by the same comparison.
	absent = "not there"
)

// proxyHealthURL is where nginx serves dokkup after an IP-mode installation.
// The port is read from the one place that decides it, because 8443 is not
// something anyone would guess and a literal here could disagree with the vhost.
var proxyHealthURL = fmt.Sprintf("https://%s:%d/api/health", installIP, proxy.DefaultTLSPort)

// fingerprintPattern matches a SHA-256 fingerprint in the colon-separated
// uppercase form a browser shows. The installer prints it that way and so does
// `openssl x509 -fingerprint`, which is the whole point of the format: one
// expression reads both, and so does an operator.
var fingerprintPattern = regexp.MustCompile(`[0-9A-F]{2}(?::[0-9A-F]{2}){31}`)

func TestInstallingAndRemovingDokkupLeavesTheDokkuHostAsItWasFound(t *testing.T) {
	dev := newDevenv(t)

	dev.build(installVersion, dev.goarch())

	// Run from outside /usr/local/bin, which is the route that makes
	// installation copy the executable rather than find it already in place --
	// and the only route that leaves something to run `uninstall` from once
	// removal has taken the installed binary away.
	installer := dev.inContainer(dev.built(installVersion))

	// Removal re-authenticates. `container exec` gives it no terminal and no
	// sudo session, so the branch taken is the host-name challenge and the
	// answer arrives on standard input -- which is what an operator scripting
	// this over ssh gets too, and the only branch a test can drive without a
	// pty.
	confirmedRemoval := fmt.Sprintf("printf '%%s\\n' \"$(hostname)\" | %s uninstall --purge",
		installer)

	// The application is created before the inventory is taken, so that "this
	// host is as it was found" is a claim about a host with something on it
	// rather than about an empty one.
	dev.mustExec(fmt.Sprintf("%s apps:create %s >/dev/null 2>&1 || true",
		dokku.DefaultBinary, probeApp))

	before := dev.inventory()

	// The fingerprint the installer printed, kept for the subtest that asks
	// nginx what it actually serves. A number an operator is told to check is
	// worse than no number at all if it is merely intended.
	var printed string

	t.Run("installs, and the service comes up answering health", func(t *testing.T) {
		dev := dev.with(t)

		stdout, stderr, code := dev.exec(installer + " install --accept-self-signed --ip " + installIP)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		printed = fingerprintIn(t, stdout)

		if got := dev.healthField("dokkup"); got != installVersion {
			t.Errorf("health reports %q, want %q", got, installVersion)
		}
		// The half that could not be produced by a process merely listening on
		// a port. "ok" is the server having run `sudo -n -u dokku
		// /usr/bin/dokku version` from inside the unit, so it is the sudoers
		// rule this installation wrote being exercised rather than inspected,
		// and the `sudo -n -u dokku` invocation form along with it.
		if got := dev.healthField("status"); got != "ok" {
			t.Errorf("health reports status %q, want \"ok\": the service cannot reach Dokku, so "+
				"either the rule or the invocation it was written for is wrong", got)
		}

		account := strings.TrimSpace(dev.mustExec("getent passwd " + hostpaths.User))
		if !strings.HasSuffix(account, ":"+host.Nologin) {
			t.Errorf("the account is %q, want the shell %s: nobody signs in as a service account",
				account, host.Nologin)
		}
		// Compared as whole fields: "dokkup" contains "dokku", so a substring
		// test would pass on an account that joined nothing.
		groups := strings.Fields(dev.mustExec("id -nG " + hostpaths.User))
		if !slices.Contains(groups, hostpaths.DokkuGroup) {
			t.Errorf("%s is in %v, which does not include %q -- the reach into DOKKU_ROOT, "+
				"which is 0750 dokku:dokku", hostpaths.User, groups, hostpaths.DokkuGroup)
		}

		paths := dev.ownedPaths()
		// Not 0755. The database, the audit trail and the private key live in
		// there, so another unprivileged account must not even be able to list
		// it.
		if got, want := paths[hostpaths.DataDir], "750:"+hostpaths.User+":"+hostpaths.Group; got != want {
			t.Errorf("%s is %s, want %s", hostpaths.DataDir, got, want)
		}
		// sudo rejects a rule owned by anyone but root and then grants nothing
		// at all -- measured on this host: `sudo: /etc/sudoers.d/zz-probe-acct
		// is owned by uid 996, should be 0`.
		if got := paths[hostpaths.Sudoers]; got != "440:root:root" {
			t.Errorf("%s is %s, want 440:root:root", hostpaths.Sudoers, got)
		}
		if _, stderr, code := dev.exec(host.Visudo + " -c"); code != 0 {
			t.Errorf("visudo -c exits %d, so this host's sudoers no longer parses as a whole "+
				"and every rule on it is at risk: %s", code, stderr)
		}

		// The rule permits one program, run as one account. Both halves are
		// asserted because a rule that permits everything also passes the first.
		permitted := fmt.Sprintf("%s -u %s -- %s -n -u %s %s version",
			host.Runuser, hostpaths.User, host.Sudo, dokku.DefaultRunAs, dokku.DefaultBinary)
		if stdout, stderr, code := dev.exec(permitted); code != 0 {
			t.Errorf("%s cannot run Dokku (%d): %s%s", hostpaths.User, code, stdout, stderr)
		}
		refused := fmt.Sprintf("%s -u %s -- %s -n -u %s /bin/ls /",
			host.Runuser, hostpaths.User, host.Sudo, dokku.DefaultRunAs)
		if _, _, code := dev.exec(refused); code == 0 {
			t.Errorf("%s may also run /bin/ls as %s; the rule is meant to name one program and "+
				"permit nothing else", hostpaths.User, dokku.DefaultRunAs)
		}

		if got := strings.TrimSpace(dev.mustExec("systemctl is-enabled " + service.Name)); got != "enabled" {
			t.Errorf("%s is %q, want \"enabled\": it would not come back after a reboot", service.Name, got)
		}

		// Through nginx, not on the loopback address the service binds. It is a
		// different question from the one health answered above, and the only
		// one an operator's browser ever asks.
		if got := dev.httpStatus(proxyHealthURL); got != "200" {
			t.Errorf("nginx answers %s at %s, want 200", got, proxyHealthURL)
		}
		if served := dev.servedFingerprint(); served != printed {
			t.Errorf("nginx serves the certificate %s, and the installer told the operator to "+
				"expect %s; the number they were asked to check is not the one they will see",
				served, printed)
		}
	})

	// Not a Done-when of its own, and the property every other one rests on: an
	// installation that cannot be repeated cannot be used to repair anything.
	t.Run("installing again changes nothing and still succeeds", func(t *testing.T) {
		dev := dev.with(t)

		was := dev.ownedPaths()

		stdout, stderr, code := dev.exec(installer + " install --accept-self-signed --ip " + installIP)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}

		now := dev.ownedPaths()
		for path, want := range was {
			// The one path a second run adds, and it is not an accident:
			// hostpaths documents the copy a reinstall keeps and the backup an
			// update keeps as the same act, and this is that act happening.
			if path == hostpaths.PreviousBinary {
				continue
			}
			if now[path] != want {
				t.Errorf("%s was %s and is now %s", path, want, now[path])
			}
		}
		if was[hostpaths.PreviousBinary] != absent || now[hostpaths.PreviousBinary] == absent {
			t.Errorf("%s was %s and is now %s, want it to appear only on the second run",
				hostpaths.PreviousBinary, was[hostpaths.PreviousBinary], now[hostpaths.PreviousBinary])
		}
		if _, _, code := dev.exec("cmp -s " + hostpaths.Binary + " " + hostpaths.PreviousBinary); code != 0 {
			t.Errorf("%s is not a copy of %s, so a rollback would put back something else",
				hostpaths.PreviousBinary, hostpaths.Binary)
		}

		if got := dev.healthField("dokkup"); got != installVersion {
			t.Errorf("health reports %q, want %q still", got, installVersion)
		}
		if got := dev.healthField("status"); got != "ok" {
			t.Errorf("health reports status %q, want \"ok\" still", got)
		}
	})

	// Done-when 4. A removal nobody confirmed must be a removal that did not
	// happen -- not one that stopped part way, which is the state an operator
	// cannot tell from a broken host.
	t.Run("an uninstall that is not confirmed removes nothing", func(t *testing.T) {
		dev := dev.with(t)

		for name, tc := range map[string]struct {
			command string
			says    string
		}{
			// --purge is what makes the typed line reach the challenge: without
			// it the data-directory question is asked first and eats the answer.
			// It is also the graver command, which is what a refusal has to hold
			// back.
			"a name that is not this host's": {
				command: fmt.Sprintf("printf 'not-this-host\\n' | %s uninstall --purge", installer),
				says:    "not confirmed",
			},
			"nothing typed at all, which is what a closed standard input is": {
				command: installer + " uninstall --purge </dev/null",
				says:    "nothing was typed",
			},
			"an answer to the data-directory question that is neither yes nor no": {
				command: fmt.Sprintf("printf 'not-this-host\\n' | %s uninstall", installer),
				says:    "is not yes or no",
			},
		} {
			t.Run(name, func(t *testing.T) {
				dev := dev.with(t)

				was := dev.ownedPaths()
				for path, spec := range was {
					if spec == absent {
						t.Fatalf("%s is already gone before the refusal, so this proves nothing", path)
					}
				}

				stdout, stderr, code := dev.exec(tc.command)
				if code == 0 {
					t.Fatalf("exit code = 0; an unconfirmed removal reported success\n%s", stdout)
				}
				if !strings.Contains(stderr, tc.says) {
					t.Errorf("stderr does not say %q: %q", tc.says, stderr)
				}

				now := dev.ownedPaths()
				for path, want := range was {
					if now[path] != want {
						t.Errorf("%s was %s and is now %s, and nobody confirmed anything",
							path, want, now[path])
					}
				}
				// Read rather than asserted through mustExec: `systemctl
				// is-active` exits 3 for a service that is not running, and that
				// answer is the interesting one here.
				if state, _, _ := dev.exec("systemctl is-active " + service.Name); strings.TrimSpace(state) != "active" {
					t.Errorf("%s is %q, want \"active\": an unconfirmed removal stopped it",
						service.Name, strings.TrimSpace(state))
				}
				if got := dev.healthField("dokkup"); got != installVersion {
					t.Errorf("health reports %q, want the untouched %q", got, installVersion)
				}
			})
		}
	})

	// Done-when 2.
	t.Run("uninstalling leaves no user, no rule, no unit, no binary and no vhost", func(t *testing.T) {
		dev := dev.with(t)

		stdout, stderr, code := dev.exec(confirmedRemoval)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}

		for path, got := range dev.ownedPaths() {
			if got != absent {
				t.Errorf("%s survived the removal: %s", path, got)
			}
		}
		if _, _, code := dev.exec("getent passwd " + hostpaths.User); code == 0 {
			t.Errorf("the system user %s is still on this host", hostpaths.User)
		}
		if _, _, code := dev.exec("getent group " + hostpaths.Group); code == 0 {
			t.Errorf("the system group %s is still on this host", hostpaths.Group)
		}

		// The group dokkup joined and never created. Every app on this host is
		// deployed through it, so removing it would be the one mistake this
		// command must never make -- and it must equally not leave dokkup in it,
		// which would be a membership pointing at a uid the host is now free to
		// hand to somebody else.
		dokkuGroup := strings.TrimSpace(dev.mustExec("getent group " + hostpaths.DokkuGroup))
		fields := strings.Split(dokkuGroup, ":")
		if len(fields) != 4 {
			t.Fatalf("getent group %s answered %q, which is not a group line",
				hostpaths.DokkuGroup, dokkuGroup)
		}
		if members := strings.Split(fields[3], ","); slices.Contains(members, hostpaths.User) {
			t.Errorf("the %s group still lists %s: %q", hostpaths.DokkuGroup, hostpaths.User, dokkuGroup)
		}

		// Asked of the whole tree rather than of the one path, because that is
		// the promise internal/proxy makes: dokkup writes one file under
		// /etc/nginx and no symlink, no sites-available entry and no Dokku
		// property, so deleting that file removes it from the proxy entirely.
		if found, _, _ := dev.exec("grep -rl " + hostpaths.User + " /etc/nginx"); strings.TrimSpace(found) != "" {
			t.Errorf("something under /etc/nginx still mentions dokkup: %q", found)
		}
		if _, stderr, code := dev.exec("nginx -t"); code != 0 {
			t.Fatalf("nginx no longer accepts this host's configuration, which is every app on "+
				"it at the next start (%d): %s", code, stderr)
		}
		if _, _, code := dev.exec("systemctl cat " + service.Name); code == 0 {
			t.Errorf("systemd still has a unit for %s", service.Name)
		}
	})

	// Done-when 3, and the whole of Done-when 2 that a path list cannot reach.
	//
	// The issue asks for an inventory that differs only by the Let's Encrypt
	// plugin the installer put there. Installation no longer installs it -- it
	// issues certificates for apps and dokkup is not an app, see ADR-0006 as
	// amended -- so the answer here is the stronger one: nothing differs at all.
	t.Run("the Dokku host is exactly as it was found", func(t *testing.T) {
		dev := dev.with(t)

		if after := dev.inventory(); after != before {
			t.Errorf("installing and removing dokkup changed this host:\n%s",
				firstDifference(before, after))
		}
	})

	// The exit codes that make removal re-runnable: userdel and groupdel both
	// exit 6 for an identity that is not there, and `systemctl disable --now`
	// exits 1 for a unit whose file is gone, so removal asks for it only while
	// there is one. The reason somebody runs this twice is that the first run
	// stopped part way, and refusing to finish would be the worst possible
	// answer to that.
	t.Run("uninstalling again is not an error", func(t *testing.T) {
		dev := dev.with(t)

		stdout, stderr, code := dev.exec(confirmedRemoval)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "There was nothing of dokkup's left on this host.") {
			t.Errorf("a host with nothing of dokkup's on it was told what was removed from it:\n%s",
				stdout)
		}
		if strings.TrimSpace(stderr) != "" {
			t.Errorf("the second removal complained about something already gone: %q", stderr)
		}
	})
}

// inventory is everything about this host that installing and removing dokkup
// has to leave alone, in one string, so that "nothing differs" is one
// comparison rather than a list of things somebody remembered to check.
//
// Every command has its stderr folded in, so a command that stops working
// arrives in the comparison as a difference rather than as silence.
func (d *devenv) inventory() string {
	d.t.Helper()

	return d.mustExec(strings.Join([]string{
		"echo '--- apps'",
		dokku.DefaultBinary + " apps:list 2>&1",
		dokku.DefaultBinary + " ps:report " + probeApp + " 2>&1",
		"echo '--- plugins'",
		dokku.DefaultBinary + " plugin:list 2>&1",
		"echo '--- proxy'",
		"ls -A /etc/nginx/conf.d 2>&1",
		"nginx -t 2>&1",
		"echo '--- identities'",
		"getent passwd 2>&1",
		"getent group 2>&1",
		"echo '--- volumes'",
		"ls -A " + hostpaths.DokkuStorageRoot + " 2>&1",
	}, "\n"))
}

// ownedPaths is the mode, owner and group of every location dokkup puts on a
// host.
//
// The list is read from hostpaths rather than restated here, so a path added to
// what installation writes arrives in these assertions on its own, and a
// removal that forgets it fails them. One invocation rather than one per path:
// entering the container costs far more than asking it another question.
func (d *devenv) ownedPaths() map[string]string {
	d.t.Helper()

	paths := append(hostpaths.Owned(), hostpaths.Conditional()...)

	script := make([]string, 0, len(paths))
	for _, path := range paths {
		script = append(script,
			fmt.Sprintf("stat -c '%%a:%%U:%%G' %s 2>/dev/null || echo '%s'", path, absent))
	}

	answers := strings.Split(strings.TrimRight(d.mustExec(strings.Join(script, "\n")), "\n"), "\n")
	if len(answers) != len(paths) {
		d.t.Fatalf("asked about %d paths and got %d answers: %q", len(paths), len(answers), answers)
	}

	found := make(map[string]string, len(paths))
	for k, path := range paths {
		found[path] = strings.TrimSpace(answers[k])
	}
	return found
}

// httpStatus reports what an HTTP request answered with, and nothing else. The
// certificate is deliberately not verified here: it is self-signed, and
// [devenv.servedFingerprint] is what checks it, against the number the operator
// was given rather than against a trust store that could never hold it.
func (d *devenv) httpStatus(url string) string {
	d.t.Helper()

	return strings.TrimSpace(d.mustExec("curl -sk -o /dev/null -w '%{http_code}' " + url))
}

// servedFingerprint is the SHA-256 of the certificate nginx actually serves,
// read off the connection the way a browser reads it rather than off the file
// installation wrote.
func (d *devenv) servedFingerprint() string {
	d.t.Helper()

	return fingerprintIn(d.t, d.mustExec(fmt.Sprintf(
		"openssl s_client -connect %s:%d </dev/null 2>/dev/null | "+
			"openssl x509 -noout -fingerprint -sha256",
		installIP, proxy.DefaultTLSPort)))
}

func fingerprintIn(t *testing.T, out string) string {
	t.Helper()

	found := fingerprintPattern.FindString(out)
	if found == "" {
		t.Fatalf("no SHA-256 fingerprint anywhere in:\n%s", out)
	}
	return found
}

// firstDifference reports the first line two inventories disagree on.
//
// Printing both in full would be sixty lines of agreement surrounding one word,
// and the operator of a host this test has just failed on needs to be told what
// changed, not shown everything that did not.
func firstDifference(before, after string) string {
	was, now := strings.Split(before, "\n"), strings.Split(after, "\n")

	for k := 0; k < max(len(was), len(now)); k++ {
		left, right := lineAt(was, k), lineAt(now, k)
		if left != right {
			return fmt.Sprintf("  line %d was %q\n  line %d is  %q", k+1, left, k+1, right)
		}
	}
	return "  the two inventories are identical, so something else failed"
}

func lineAt(lines []string, k int) string {
	if k >= len(lines) {
		return "(nothing: the inventory is shorter than it was)"
	}
	return lines[k]
}
