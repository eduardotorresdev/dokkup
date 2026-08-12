//go:build integration

package dokku_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/dokku"
)

// This is [dokku.ExecClient] against a real Dokku, with a real app deployed on
// it, real sudo and the real `ps:report`, `domains:report` and `logs`.
//
// The unit tests prove the argument vectors and the parsing against a stub.
// What a stub cannot be convincingly wrong about is any of this: that
// `ps:report --format json` really carries the scale as one `status-<type>.<n>`
// key per container; that `domains:report --format json` really answers with the
// vhosts space-separated in one field; that the log lines really arrive wrapped
// in colour codes nothing can turn off; and that abandoning a followed stream
// really leaves no `docker logs --follow` behind on the host.
//
// It runs inside the dev environment rather than against it. The client invokes
// a local binary, so the only way to exercise it against the real Dokku is to be
// where the real Dokku is: this test cross-compiles itself for the container,
// runs itself in there through the bind mount, and relays what it said. Driving
// Dokku from the host through `container exec` would prove less than nothing
// about the streaming path, because that transport buffers the output it
// forwards and does not pass a signal on to the process it started.
//
// Nothing here runs in parallel: there is one dev environment and one probe app
// on it, and the subtests are one deployment read in order.

const (
	devenvName = "dokkup-devenv"

	// probeApp is deployed by this test and destroyed with it. The name is its
	// own, so that nothing it does can be mistaken for an operator's app or for
	// the app the installer tests create.
	probeApp = "dokkup-dokku-probe"

	// probeImage is built in the dev environment and deployed from there, so
	// that no part of this test depends on a builder or a git remote.
	probeImage = "dokkup-dokku-probe:v1"

	// innerEnv marks the run that is already inside the container. Without it
	// the test would launch itself for ever.
	innerEnv = "DOKKUP_DEVENV_INNER"

	// workspace is where the dev environment sees this repository.
	workspace = "/workspace"

	// stagingDir is under dist/, which is git-ignored, removed by `make clean`,
	// and inside the bind mount the container sees as workspace.
	stagingDir = "dist/integration"

	// probeScale is how many web containers the app is deployed with. Two
	// rather than one because the scale is what is being read, and a count of
	// one is the answer a bug that counts process types rather than containers
	// would also give.
	probeScale = 2
)

// logLine matches a line as Dokku 0.38.7 writes it once the colour codes are
// gone: an RFC 3339 timestamp, the container that wrote it, then the app's own
// output.
var logLine = regexp.MustCompile(`^\S+Z app\[(\w+)\.\d+\]: `)

func TestDokkuClientAgainstARealDokku(t *testing.T) {
	if os.Getenv(innerEnv) == "" {
		runInsideTheDevenv(t)
		return
	}

	// The client an installed dokkup builds: the real binary, reached through
	// sudo as the account the sudoers rule names.
	client := dokku.NewExecClient()

	t.Run("the scale comes back as one count per process type", func(t *testing.T) {
		types, err := client.ProcessTypes(t.Context(), probeApp)
		if err != nil {
			t.Fatalf("ProcessTypes: %v", err)
		}
		want := map[string]int{"web": probeScale}
		if len(types) != len(want) || types["web"] != want["web"] {
			t.Errorf("process types = %v, want %v", types, want)
		}
	})

	t.Run("an app with nothing running has no process types and is not an error", func(t *testing.T) {
		types, err := client.ProcessTypes(t.Context(), probeApp+"-empty")
		if err != nil {
			t.Fatalf("ProcessTypes: %v", err)
		}
		if len(types) != 0 {
			t.Errorf("process types = %v, want none for an app that was never deployed", types)
		}
	})

	t.Run("the domains Dokku serves the app at come back", func(t *testing.T) {
		domains, err := client.Domains(t.Context(), probeApp)
		if err != nil {
			t.Fatalf("Domains: %v", err)
		}
		want := []string{probeApp + ".example.com", "alt." + probeApp + ".example.com"}
		if !slices.Equal(domains, want) {
			t.Errorf("domains = %v, want %v", domains, want)
		}
	})

	t.Run("an app with no domain of its own answers with none", func(t *testing.T) {
		domains, err := client.Domains(t.Context(), probeApp+"-empty")
		if err != nil {
			t.Fatalf("Domains: %v", err)
		}
		if len(domains) != 0 {
			t.Errorf("domains = %v, want none", domains)
		}
	})

	t.Run("log lines come back parsed and without the colour codes", func(t *testing.T) {
		var lines []string
		opts := dokku.LogOptions{Tail: 5}
		if err := client.Logs(t.Context(), probeApp, opts, collect(&lines)); err != nil {
			t.Fatalf("Logs: %v", err)
		}
		if len(lines) == 0 {
			t.Fatal("no log lines came back from an app that writes one every two seconds")
		}
		for _, line := range lines {
			if strings.ContainsRune(line, 0x1b) {
				t.Errorf("line %q still carries the escape codes Dokku colours it with", line)
			}
			if !logLine.MatchString(line) {
				t.Errorf("line %q is not a log line as Dokku writes them", line)
			}
		}
	})

	t.Run("a stream narrowed to a process type carries only that one", func(t *testing.T) {
		var lines []string
		opts := dokku.LogOptions{ProcessType: "web", Tail: 5}
		if err := client.Logs(t.Context(), probeApp, opts, collect(&lines)); err != nil {
			t.Fatalf("Logs: %v", err)
		}
		if len(lines) == 0 {
			t.Fatal("no log lines came back for the process type the app is running")
		}
		for _, line := range lines {
			if match := logLine.FindStringSubmatch(line); match == nil || match[1] != "web" {
				t.Errorf("line %q came from a process type the stream did not ask for", line)
			}
		}
	})

	t.Run("abandoning a followed stream leaves no process behind on the host", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		// Compared against what was already running rather than against
		// nothing: the dev environment is a host someone works on, and a log
		// this test never opened is not this test's to account for.
		before := followers(t)

		delivered := 0
		err := client.Logs(ctx, probeApp, dokku.LogOptions{Follow: true, Tail: 1}, func(string) error {
			delivered++
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Logs error = %v, want context.Canceled", err)
		}
		if delivered == 0 {
			t.Error("the followed stream ended without delivering a line")
		}

		// Dokku follows a log by running `docker logs --follow` per container,
		// under a tree of shells. Killing only the process dokkup started
		// leaves every one of them running, reparented to init, following an
		// app nobody is watching any more.
		//
		// Polled because the kill is delivered as the stream returns and the
		// kernel reaps in its own time.
		var left []string
		for range 50 {
			left = added(before, followers(t))
			if len(left) == 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if len(left) != 0 {
			t.Errorf("an abandoned stream left this running:\n%s", strings.Join(left, "\n"))
		}
	})

	t.Run("an app Dokku does not have is reported as a command error", func(t *testing.T) {
		_, err := client.ProcessTypes(t.Context(), probeApp+"-missing")

		var commandErr *dokku.CommandError
		if !errors.As(err, &commandErr) {
			t.Fatalf("ProcessTypes error = %v (%T), want a *dokku.CommandError", err, err)
		}
		if !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("the error drops what Dokku said: %v", err)
		}
	})
}

// collect returns a callback that keeps every line it is given.
func collect(lines *[]string) func(string) error {
	return func(line string) error {
		*lines = append(*lines, line)
		return nil
	}
}

// followers reports the log-following processes running on the host: the dokku
// shells that a `logs` invocation is made of, and the `docker logs --follow`
// they end in.
//
// Each one is reported with its process id in front. Two of these processes have
// identical argument vectors as a matter of course -- Dokku runs the same
// `docker logs --follow` under three nested shells -- so comparing what is
// running now against what was running before by command line alone would hide
// a leak behind an identical process left by an earlier run. The process id is
// what makes them countable.
func followers(t *testing.T) []string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "ps", "-e", "-o", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("reading the process table: %v", err)
	}

	var running []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "docker logs") || strings.Contains(line, "dokku logs") {
			running = append(running, strings.TrimSpace(line))
		}
	}
	return running
}

// added reports the processes in now that were not in before.
func added(before, now []string) []string {
	was := make(map[string]bool, len(before))
	for _, line := range before {
		was[line] = true
	}

	var extra []string
	for _, line := range now {
		if !was[line] {
			extra = append(extra, line)
		}
	}
	return extra
}

// runInsideTheDevenv deploys the probe app, builds this test for the container,
// and runs it in there.
func runInsideTheDevenv(t *testing.T) {
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

	dev := &devenv{t: t, cli: cli}
	if _, code := dev.exec("true"); code != 0 {
		t.Skipf("%s is not running; run 'make devenv-up' first", devenvName)
	}

	dev.deployProbeApp()
	t.Cleanup(dev.destroyProbeApp)

	binary := dev.build()
	out, code := dev.exec(fmt.Sprintf("%s=1 %s -test.run %s -test.v",
		innerEnv, filepath.Join(workspace, binary), t.Name()))

	// Relayed rather than summarised: what failed inside the container is what
	// the person running `make test-integration` needs to read.
	t.Log("\n" + strings.TrimSpace(out))
	if code != 0 {
		t.Fatalf("the client failed against the real Dokku in %s (exit %d)", devenvName, code)
	}
}

// devenv runs scripts in the development container.
type devenv struct {
	t   *testing.T
	cli string
}

// exec runs a shell script inside the container and returns what it said on
// both streams together, so that a failure is read in the order it happened.
func (d *devenv) exec(script string) (output string, code int) {
	d.t.Helper()
	return d.execContext(d.t.Context(), script)
}

// execContext is exec with the context spelled out, because teardown runs after
// the test's own context has been cancelled and still has an app to destroy.
func (d *devenv) execContext(ctx context.Context, script string) (output string, code int) {
	d.t.Helper()

	cmd := exec.CommandContext(ctx, d.cli, "exec", devenvName, "sh", "-c", script)
	var combined strings.Builder
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			d.t.Fatalf("running %q in %s: %v", script, devenvName, err)
		}
		code = exit.ExitCode()
	}
	return combined.String(), code
}

func (d *devenv) mustExec(script string) string {
	d.t.Helper()

	out, code := d.exec(script)
	if code != 0 {
		d.t.Fatalf("%q failed with %d: %s", script, code, out)
	}
	return out
}

// deployProbeApp puts an app on the host that writes a line every two seconds,
// serves two domains, and runs two containers of one process type.
//
// It is deployed from an image built in the container rather than pushed over
// git, because what this test needs is an app whose output never stops, and the
// shortest way to one is a Dockerfile with a loop in it. The image is built on
// nginx:alpine because the dev environment already has it, so a rebuild costs
// no network.
func (d *devenv) deployProbeApp() {
	d.t.Helper()

	d.destroyProbeApp()
	d.mustExec(fmt.Sprintf("dokku apps:create %s && dokku apps:create %s-empty", probeApp, probeApp))
	d.mustExec(fmt.Sprintf("dokku domains:add %s %s.example.com && dokku domains:add %s alt.%s.example.com",
		probeApp, probeApp, probeApp, probeApp))

	d.mustExec(strings.Join([]string{
		"set -e",
		"mkdir -p /root/" + probeApp,
		"cd /root/" + probeApp,
		"docker image inspect nginx:alpine >/dev/null 2>&1 || docker pull nginx:alpine",
		"cat > Dockerfile <<'EOF'",
		"FROM nginx:alpine",
		`CMD ["/bin/sh","-c","i=0; while true; do i=$((i+1)); echo \"probe line $i\"; sleep 2; done"]`,
		"EOF",
		"docker build -t " + probeImage + " .",
	}, "\n"))

	d.mustExec("dokku git:from-image " + probeApp + " " + probeImage)
	d.mustExec(fmt.Sprintf("dokku ps:scale %s web=%d", probeApp, probeScale))

	// The app has to have said something before its logs can be read, and it
	// says something every two seconds.
	time.Sleep(3 * time.Second)
}

func (d *devenv) destroyProbeApp() {
	d.t.Helper()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.t.Context()), 5*time.Minute)
	defer cancel()

	d.execContext(ctx, fmt.Sprintf("dokku --force apps:destroy %s; dokku --force apps:destroy %s-empty",
		probeApp, probeApp))
}

// build cross-compiles this test for the container's architecture, into the
// bind mount the container reads the repository through.
func (d *devenv) build() string {
	d.t.Helper()

	var goarch string
	switch machine := strings.TrimSpace(d.mustExec("uname -m")); machine {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		d.t.Skipf("no Go toolchain target for %s", machine)
	}

	output := filepath.Join(stagingDir, "dokku.test")
	root, err := filepath.Abs("../..")
	if err != nil {
		d.t.Fatalf("locating the repository: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, stagingDir), 0o755); err != nil {
		d.t.Fatalf("preparing the staging directory: %v", err)
	}

	cmd := exec.CommandContext(d.t.Context(), "go", "test", "-c",
		"-tags", "integration", "-o", output, "./internal/dokku")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+goarch)

	if out, err := cmd.CombinedOutput(); err != nil {
		d.t.Fatalf("building this test for linux/%s: %v\n%s", goarch, err, out)
	}
	return output
}
