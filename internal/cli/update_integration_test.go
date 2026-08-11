//go:build integration

package cli_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/dokku"
	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/service"
)

// This is `dokkup update` against a real systemd, on a host where dokkup is
// really installed, updating from one really-built binary to another.
//
// The unit tests prove the decisions; this proves the assumptions underneath
// them -- that a running executable can be replaced by renaming over it, that
// systemd restarts into the new one, that the health endpoint answers as the
// version that was just installed, and that a version which never comes up is
// undone rather than left in place. None of those are things a fake can be
// wrong about convincingly.
//
// The release is served from inside the container on loopback, so nothing
// depends on host-to-container networking and it behaves the same under Apple's
// container runtime and under Docker.

const (
	devenvName = "dokkup-devenv"

	// releaseOrigin is where the fake release is served. The binaries under
	// test are linked to reach for it instead of github.com.
	releaseOrigin = "http://127.0.0.1:8000"

	// stagingDir is under dist/, which is git-ignored and removed by `make
	// clean`, and is inside the bind mount the container sees as /workspace.
	stagingDir = "dist/integration"

	fromVersion   = "v0.1.0"
	toVersion     = "v0.1.1"
	brokenVersion = "v0.1.2"

	// probeApp is a real Dokku application, created so that "can the service see
	// Dokku's state" has an answer that is not vacuously true on an empty host.
	probeApp = "dokkup-integration-probe"
)

// healthURL is where the installed service answers, derived from the same
// default the unit is rendered with rather than repeated as a literal.
var healthURL = "http://" + service.DefaultListen + "/api/health"

func TestUpdateReplacesARunningDokkupAndUndoesOneThatDoesNotComeUp(t *testing.T) {
	host := newDevenv(t)

	goarch := host.goarch()
	asset := "dokkup_linux_" + goarch

	// Two real dokkup binaries, linked to fetch releases from the loopback
	// server rather than from GitHub. A released binary never carries these
	// flags; internal/release has a test asserting the compiled-in defaults.
	host.build(fromVersion, goarch)
	host.build(toVersion, goarch)

	host.publish(toVersion, asset, host.built(toVersion))

	// A "version that does not come up" needs no special program: anything
	// systemd can execute and that exits immediately will do, and the test
	// computes its checksum itself so verification passes and the failure lands
	// where it is meant to -- at the health gate, not at the download.
	broken := filepath.Join(host.staging, "broken")
	host.write(broken, "#!/bin/sh\nexit 1\n")
	host.publish(brokenVersion, asset, broken)

	host.install(fromVersion)
	host.serveReleases()

	t.Run("the health endpoint reports which dokkup is running", func(t *testing.T) {
		if got := host.servingVersion(); got != fromVersion {
			t.Fatalf("health reports %q, want %q", got, fromVersion)
		}
	})

	// The unit's sandbox is inherited by the dokku process the service runs, and
	// every way of getting that wrong is silent: `dokku version` keeps answering
	// under all of them, so health says "ok" while the interface shows an empty
	// host. Asking the service to list apps is what makes such a mistake fail
	// here rather than in production. See ADR-0012.
	t.Run("the service can actually reach Dokku through the unit", func(t *testing.T) {
		status, apps := host.dokkuThroughService()

		if status != "ok" {
			t.Errorf("health reports %q; the service cannot reach Dokku under this unit", status)
		}
		if !strings.Contains(apps, probeApp) {
			t.Errorf("the service lists %q, which does not include the app that exists (%s);\n"+
				"a sandbox directive in the unit is hiding Dokku's state", apps, probeApp)
		}
	})

	t.Run("check reports the newer version and changes nothing", func(t *testing.T) {
		before := host.installedSum()

		stdout, _, code := host.dokkup("update", "--check")

		if code != 0 {
			t.Errorf("exit code = %d, want 0: an available update is news, not a failure", code)
		}
		if !strings.Contains(stdout, "0.1.1 is available") {
			t.Errorf("--check did not report the available version: %q", stdout)
		}
		if after := host.installedSum(); after != before {
			t.Error("--check changed the installed binary")
		}
		if got := host.servingVersion(); got != fromVersion {
			t.Errorf("--check disturbed the service; it now reports %q", got)
		}
	})

	t.Run("a download that does not match its checksum installs nothing", func(t *testing.T) {
		before := host.installedSum()
		restore := host.corruptChecksums(toVersion)
		defer restore()

		_, stderr, code := host.dokkup("update")

		if code == 0 {
			t.Fatal("a binary that failed verification was installed")
		}
		if !strings.Contains(stderr, "checksum") {
			t.Errorf("stderr does not say what was wrong: %q", stderr)
		}
		if after := host.installedSum(); after != before {
			t.Error("the installed binary was replaced despite a checksum mismatch")
		}
		// The service must not even have been restarted: nothing got far enough
		// for that to be sensible.
		if got := host.servingVersion(); got != fromVersion {
			t.Errorf("the service reports %q, want the untouched %q", got, fromVersion)
		}
	})

	t.Run("a verified update replaces the running binary and comes back healthy", func(t *testing.T) {
		stdout, stderr, code := host.dokkup("update", "--timeout", "30s")

		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if got := host.dokkupVersion(); !strings.Contains(got, toVersion) {
			t.Errorf("the installed binary reports %q, want %q", got, toVersion)
		}
		// The point of the whole exercise: the service is serving the new one,
		// not merely running.
		if got := host.servingVersion(); got != toVersion {
			t.Errorf("health reports %q, want %q", got, toVersion)
		}
		// Kept so the next update has something to roll back to.
		if _, _, code := host.exec("test -f " + hostpaths.PreviousBinary); code != 0 {
			t.Error("the previous binary was not kept")
		}
		if out, _, _ := host.exec("ls -A " + filepath.Dir(hostpaths.Binary)); strings.Contains(out, ".dokkup.new-") {
			t.Errorf("a partial download was left beside the binary: %q", out)
		}
	})

	t.Run("check then reports the host is up to date, still changing nothing", func(t *testing.T) {
		before := host.installedSum()

		stdout, _, code := host.dokkup("update", "--check")

		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
		if !strings.Contains(stdout, "newest release") {
			t.Errorf("--check did not report the host as current: %q", stdout)
		}
		if after := host.installedSum(); after != before {
			t.Error("--check changed the installed binary")
		}
	})

	// The one the issue asks to be forced. A version that installs, passes
	// verification, and then never answers must leave the host on the version
	// that was working.
	t.Run("a version that never comes up is rolled back", func(t *testing.T) {
		stdout, stderr, code := host.dokkup("update", "--version", brokenVersion, "--timeout", "15s")

		if code != 4 {
			t.Fatalf("exit code = %d, want 4\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if !strings.Contains(stderr, "rolled back") {
			t.Errorf("stderr does not say the update was rolled back: %q", stderr)
		}
		if got := host.dokkupVersion(); !strings.Contains(got, toVersion) {
			t.Errorf("the installed binary reports %q, want the restored %q", got, toVersion)
		}
		// systemd had given up on the crash-looping unit by now, so this also
		// proves the rollback clears that before restarting.
		if got := host.servingVersion(); got != toVersion {
			t.Errorf("health reports %q, want the restored %q serving again", got, toVersion)
		}
	})
}

// devenv is the running development environment, driven from outside.
type devenv struct {
	t       *testing.T
	cli     string
	root    string
	staging string
}

func newDevenv(t *testing.T) *devenv {
	t.Helper()

	cli := ""
	for _, candidate := range []string{"container", "docker"} {
		if _, err := exec.LookPath(candidate); err == nil {
			cli = candidate
			break
		}
	}
	if cli == "" {
		t.Skip("no container runtime found; run 'make devenv-up' first")
	}

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("locating the repository: %v", err)
	}

	host := &devenv{t: t, cli: cli, root: root, staging: filepath.Join(root, stagingDir)}

	if _, _, code := host.exec("true"); code != 0 {
		t.Skipf("%s is not running; run 'make devenv-up' first", devenvName)
	}

	if err := os.MkdirAll(host.staging, 0o755); err != nil {
		t.Fatalf("preparing the staging directory: %v", err)
	}
	t.Cleanup(host.teardown)

	return host
}

// exec runs a shell script inside the container and returns what it said.
func (d *devenv) exec(script string) (stdout, stderr string, code int) {
	d.t.Helper()

	stdout, stderr, code, err := d.execContext(d.t.Context(), script)
	if err != nil {
		d.t.Fatalf("running %q in %s: %v", script, devenvName, err)
	}
	return stdout, stderr, code
}

// execContext is exec with the context spelled out, because teardown runs after
// the test's own context has been cancelled and still has a host to clean up.
// A non-nil error means the script never ran; a non-zero code means it ran and
// said so.
func (d *devenv) execContext(ctx context.Context, script string) (stdout, stderr string, code int, err error) {
	cmd := exec.CommandContext(ctx, d.cli, "exec", devenvName, "sh", "-c", script)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	if runErr := cmd.Run(); runErr != nil {
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) {
			return out.String(), errOut.String(), 0, runErr
		}
		code = exit.ExitCode()
	}
	return out.String(), errOut.String(), code, nil
}

func (d *devenv) mustExec(script string) string {
	d.t.Helper()

	stdout, stderr, code := d.exec(script)
	if code != 0 {
		d.t.Fatalf("%q failed with %d: %s%s", script, code, stdout, stderr)
	}
	return stdout
}

// dokkup runs the installed binary, which is the one under test.
func (d *devenv) dokkup(args ...string) (stdout, stderr string, code int) {
	d.t.Helper()
	return d.exec(hostpaths.Binary + " " + strings.Join(args, " "))
}

func (d *devenv) goarch() string {
	d.t.Helper()

	switch machine := strings.TrimSpace(d.mustExec("uname -m")); machine {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		d.t.Skipf("no dokkup release is published for %s", machine)
		return ""
	}
}

func (d *devenv) built(version string) string {
	return filepath.Join(d.staging, "build", "dokkup-"+version)
}

// build cross-compiles a real dokkup, linked to fetch releases from the local
// server. This is the same code the release pipeline builds; only where it
// looks for releases differs.
func (d *devenv) build(version, goarch string) {
	d.t.Helper()

	output := d.built(version)
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		d.t.Fatalf("preparing the build directory: %v", err)
	}

	const pkg = "github.com/eduardotorresdev/dokkup/internal/release"
	ldflags := strings.Join([]string{
		"-s", "-w",
		"-X main.version=" + version,
		"-X main.commit=integration",
		"-X main.date=integration",
		"-X " + pkg + ".defaultBaseURL=" + releaseOrigin + "/releases",
		"-X " + pkg + ".defaultAPIURL=" + releaseOrigin + "/api",
	}, " ")

	cmd := exec.CommandContext(d.t.Context(), "go", "build",
		"-trimpath", "-ldflags", ldflags, "-o", output, "./cmd/dokkup")
	cmd.Dir = d.root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+goarch)

	if out, err := cmd.CombinedOutput(); err != nil {
		d.t.Fatalf("building dokkup %s for linux/%s: %v\n%s", version, goarch, err, out)
	}
}

// publish stages a release the way GitHub presents one: the metadata the API
// answers with, the asset, and a SHA256SUMS the test computes itself.
func (d *devenv) publish(version, asset, binary string) {
	d.t.Helper()

	content, err := os.ReadFile(binary)
	if err != nil {
		d.t.Fatalf("reading %s: %v", binary, err)
	}

	dir := filepath.Join(d.staging, "release", "releases", "download", version)
	d.write(filepath.Join(dir, asset), string(content))

	sum := sha256.Sum256(content)
	d.write(filepath.Join(dir, "SHA256SUMS"),
		fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), asset))

	metadata := fmt.Sprintf(`{"tag_name":%q,"html_url":%q,"draft":false}`+"\n",
		version, releaseOrigin+"/releases/tag/"+version)
	d.write(filepath.Join(d.staging, "release", "api", "releases", "tags", version), metadata)

	// Only the version being updated to is the newest; the broken one is
	// reached by pinning it, so that arriving at it is always deliberate.
	if version == toVersion {
		d.write(filepath.Join(d.staging, "release", "api", "releases", "latest"), metadata)
	}
}

func (d *devenv) write(path, content string) {
	d.t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		d.t.Fatalf("preparing %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		d.t.Fatalf("writing %s: %v", path, err)
	}
}

// corruptChecksums breaks a release's SHA256SUMS and returns the undo.
func (d *devenv) corruptChecksums(version string) func() {
	d.t.Helper()

	path := filepath.Join(d.staging, "release", "releases", "download", version, "SHA256SUMS")
	original, err := os.ReadFile(path)
	if err != nil {
		d.t.Fatalf("reading %s: %v", path, err)
	}

	asset := strings.Fields(string(original))[1]
	d.write(path, fmt.Sprintf("%s  %s\n", strings.Repeat("a", 64), asset))

	return func() {
		if err := os.WriteFile(path, original, 0o644); err != nil {
			d.t.Fatalf("restoring %s: %v", path, err)
		}
	}
}

// install puts dokkup on the host the way installation will: the binary, the
// service user, its data directory, and the unit rendered from the one
// definition of it, so this test cannot drift from what gets installed.
func (d *devenv) install(version string) {
	d.t.Helper()

	unitPath := filepath.Join(d.staging, "dokkup.service")
	d.write(unitPath, service.UnitFile(service.UnitConfig{}))

	inContainer := func(path string) string {
		return filepath.Join("/workspace", strings.TrimPrefix(path, d.root+"/"))
	}

	d.mustExec(strings.Join([]string{
		"set -eu",
		"groupadd -f " + hostpaths.Group,
		fmt.Sprintf("id -u %s >/dev/null 2>&1 || useradd -r -g %s -s /usr/sbin/nologin -d %s %s",
			hostpaths.User, hostpaths.Group, hostpaths.DataDir, hostpaths.User),
		// The parts of installation that let dokkup reach Dokku at all. Without
		// them the service answers 503 for a reason that has nothing to do with
		// the update, and every sandbox in the unit could be wrong without this
		// test noticing -- which is exactly how ProtectHome and
		// ProtectSystem=strict survived in it. See ADR-0012.
		fmt.Sprintf("usermod -aG %s %s", hostpaths.DokkuGroup, hostpaths.User),
		fmt.Sprintf("printf '%s ALL=(ALL) NOPASSWD:SETENV: %s\\n' > %s",
			hostpaths.User, dokku.DefaultBinary, hostpaths.Sudoers),
		"chmod 0440 " + hostpaths.Sudoers,
		// ProtectSystem=strict refuses to enter its namespace when a
		// ReadWritePaths entry is missing, so the directory comes first.
		"mkdir -p " + hostpaths.DataDir,
		fmt.Sprintf("chown %s:%s %s", hostpaths.User, hostpaths.Group, hostpaths.DataDir),
		fmt.Sprintf("install -m 0755 %s %s", inContainer(d.built(version)), hostpaths.Binary),
		fmt.Sprintf("install -m 0644 %s %s", inContainer(unitPath), hostpaths.Unit),
		"systemctl daemon-reload",
		"systemctl reset-failed " + service.Name + " 2>/dev/null || true",
		"systemctl enable " + service.Name,
		// Restart rather than `enable --now`: a unit left active by an
		// interrupted run would otherwise keep serving whichever binary it
		// started with, and the test would compare against the wrong version.
		"systemctl restart " + service.Name,
		// An application to look for. On an empty host, "the service can see
		// Dokku's state" is true of a sandbox that hides all of it.
		fmt.Sprintf("%s apps:create %s >/dev/null 2>&1 || true", dokku.DefaultBinary, probeApp),
	}, "\n"))

	// Deliberately not `curl -f`. The devenv has no Dokku reachable from the
	// dokkup user, so health correctly answers 503 degraded, and -f would read
	// that as no answer at all. Any HTTP reply proves the service is up, which
	// is the whole question here.
	d.waitFor("the service to answer", "curl -sS -o /dev/null "+healthURL)
}

// serveReleases starts the fake release server inside the container, as a
// transient unit so that it is stopped by name rather than by hunting a pid.
func (d *devenv) serveReleases() {
	d.t.Helper()

	dir := filepath.Join("/workspace", stagingDir, "release")
	d.mustExec(strings.Join([]string{
		"systemctl stop dokkup-test-release.service 2>/dev/null || true",
		fmt.Sprintf("systemd-run --unit=dokkup-test-release --collect -p WorkingDirectory=%s "+
			"/usr/bin/python3 -m http.server 8000 --bind 127.0.0.1", dir),
	}, "\n"))

	d.waitFor("the release server", "curl -fsS -o /dev/null "+releaseOrigin+"/api/releases/latest")
}

// waitFor polls a shell condition inside the container until it holds.
func (d *devenv) waitFor(what, condition string) {
	d.t.Helper()

	script := fmt.Sprintf("for _ in $(seq 1 60); do if %s; then exit 0; fi; sleep 1; done; exit 1", condition)
	if _, _, code := d.exec(script); code != 0 {
		status, _, _ := d.exec("systemctl status " + service.Name + " --no-pager -l | tail -30")
		d.t.Fatalf("waiting for %s timed out\n%s", what, status)
	}
}

// servingVersion asks the health endpoint which dokkup answered. A degraded
// reply still counts: Dokku's own health is not what is under test here.
func (d *devenv) servingVersion() string {
	d.t.Helper()
	return d.healthField("dokkup")
}

func (d *devenv) healthField(name string) string {
	d.t.Helper()

	out := d.mustExec("curl -sS " + healthURL)
	_, rest, found := strings.Cut(out, `"`+name+`":"`)
	if !found {
		d.t.Fatalf("the health endpoint said nothing about %q: %q", name, out)
	}
	value, _, _ := strings.Cut(rest, `"`)
	return value
}

// dokkuThroughService reports whether the service can reach Dokku, and what
// Dokku looks like from inside the unit's sandbox.
//
// The sandbox is taken from the rendered unit rather than restated here, so a
// directive added to [service.UnitFile] is a directive this test runs under. It
// has to be applied by hand because the answer needed is Dokku's app list, and
// dokkup does not serve one yet -- the health endpoint only ever runs `dokku
// version`, which succeeds under every sandbox that breaks everything else.
func (d *devenv) dokkuThroughService() (status, apps string) {
	d.t.Helper()

	var props []string
	for line := range strings.SplitSeq(service.UnitFile(service.UnitConfig{}), "\n") {
		directive := strings.TrimSpace(line)
		for _, sandbox := range []string{
			"User=", "Group=", "NoNewPrivileges=", "ProtectSystem=",
			"ProtectHome=", "PrivateTmp=", "ReadWritePaths=",
		} {
			if strings.HasPrefix(directive, sandbox) {
				props = append(props, "-p "+directive)
			}
		}
	}
	if len(props) == 0 {
		d.t.Fatal("no sandbox directives found in the unit, so this test would prove nothing")
	}

	out, stderr, code := d.exec(fmt.Sprintf(
		"systemd-run --pipe --wait --quiet --collect %s %s apps:list",
		strings.Join(props, " "), dokku.DefaultBinary))
	if code != 0 {
		d.t.Fatalf("running dokku under the unit's sandbox failed (%d): %s%s", code, out, stderr)
	}

	return d.healthField("status"), out
}

func (d *devenv) dokkupVersion() string {
	d.t.Helper()

	stdout, _, code := d.dokkup("version")
	if code != 0 {
		d.t.Fatalf("dokkup version exited %d: %s", code, stdout)
	}
	return strings.TrimSpace(stdout)
}

func (d *devenv) installedSum() string {
	d.t.Helper()
	return strings.Fields(d.mustExec("sha256sum " + hostpaths.Binary))[0]
}

// teardown removes what the test put on the host. It leaves Dokku, and
// everything Dokku is running, exactly as it found them.
func (d *devenv) teardown() {
	// Its own context: by the time cleanup runs, the test's has been cancelled,
	// and a leftover unit and binary on a shared devenv would break the next run.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.t.Context()), 2*time.Minute)
	defer cancel()

	_, _, _, err := d.execContext(ctx, strings.Join([]string{
		"systemctl stop dokkup-test-release.service 2>/dev/null || true",
		"systemctl disable --now " + service.Name + " 2>/dev/null || true",
		"rm -f " + hostpaths.Unit,
		"systemctl daemon-reload",
		"systemctl reset-failed " + service.Name + " 2>/dev/null || true",
		"rm -f " + hostpaths.Binary + " " + hostpaths.PreviousBinary,
		"rm -f " + hostpaths.Sudoers,
		"rm -rf " + hostpaths.DataDir,
		fmt.Sprintf("%s --force apps:destroy %s >/dev/null 2>&1 || true", dokku.DefaultBinary, probeApp),
		"userdel " + hostpaths.User + " 2>/dev/null || true",
		"groupdel " + hostpaths.Group + " 2>/dev/null || true",
	}, "\n"))
	if err != nil {
		d.t.Errorf("cleaning up %s left it dirty: %v", devenvName, err)
	}

	_ = os.RemoveAll(d.staging)
}
