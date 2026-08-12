package dokku_test

import (
	"bufio"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/dokku"
	"github.com/eduardotorresdev/dokkup/internal/stubprog"
)

// stubDokku writes a stand-in for the dokku binary that records the argument
// vector it was given and prints body. It is how the argument vectors and the
// output parsing get tested on a machine with no Dokku on it -- which is where
// they were wrong: `apps:list --quiet` was sent for the whole of this project's
// life, and Dokku 0.38.7 answers `unknown flag: --quiet` and exits 2.
func stubDokku(t *testing.T, body string) (*dokku.ExecClient, func() string) {
	t.Helper()

	dir := t.TempDir()
	binary := filepath.Join(dir, "dokku")
	log := filepath.Join(dir, "args")

	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\ncat <<'DOKKU_EOF'\n" + body + "\nDOKKU_EOF\n"
	if err := stubprog.Write(binary, script); err != nil {
		t.Fatalf("writing the stub: %v", err)
	}

	recorded := func() string {
		content, err := os.ReadFile(log)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(content))
	}

	return &dokku.ExecClient{Binary: binary}, recorded
}

// stubProgram writes a stand-in for a program dokkup spawns, recording the
// argument vector it was given. It is stubDokku without the client, for the
// tests that need to watch two programs at once: when an account is named it is
// sudo that dokkup starts and sudo that receives the whole vector, so the
// stand-in for the Dokku binary never runs and has nothing to tell.
func stubProgram(t *testing.T, name, body string) (string, func() string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, name)
	log := filepath.Join(dir, "args")

	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\n" + body + "\n"
	if err := stubprog.Write(path, script); err != nil {
		t.Fatalf("writing the stub: %v", err)
	}

	recorded := func() string {
		content, err := os.ReadFile(log)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(content))
	}

	return path, recorded
}

// The exact bytes Dokku 0.38.7 prints, header and all.
const appsListOutput = "=====> My Apps\nblog\nprobe\n"

func TestAppsAsksForNothingDokkuDoesNotUnderstand(t *testing.T) {
	t.Parallel()

	client, recorded := stubDokku(t, appsListOutput)

	if _, err := client.Apps(context.Background()); err != nil {
		t.Fatalf("Apps: %v", err)
	}

	// `apps:list` takes no flags. Sending one makes Dokku print usage and exit 2,
	// so the App list would be empty on every host.
	if got := recorded(); got != "apps:list" {
		t.Errorf("dokku was asked %q, want %q", got, "apps:list")
	}
}

func TestAppsReadsTheNamesAndNotDokkusNarration(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		output string
		want   []string
	}{
		"the real output": {output: appsListOutput, want: []string{"blog", "probe"}},
		// Dokku says this instead of printing nothing at all, and it is not an
		// application called "You haven't deployed any applications yet".
		"a host with no apps": {
			output: "=====> My Apps\n !     You haven't deployed any applications yet\n",
			want:   nil,
		},
		"blank lines": {output: "=====> My Apps\n\nblog\n\n", want: []string{"blog"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, _ := stubDokku(t, tc.output)

			apps, err := client.Apps(context.Background())
			if err != nil {
				t.Fatalf("Apps: %v", err)
			}
			if !slices.Equal(apps, tc.want) {
				t.Errorf("apps = %v, want %v", apps, tc.want)
			}
		})
	}
}

// A Dokku that has hung must not hold the HTTP request that asked open. The
// timeout alone does not achieve that: it kills dokku and nothing else, while
// Wait goes on waiting for the stdout pipe -- which every process dokku forked,
// and it forks docker, git and plugin hooks, is still holding.
//
// Like its counterpart in internal/service, this only bites on Linux. macOS
// closes the pipes as the process dies and reports the timeout honestly.
func TestAHungDokkuIsGivenUpOn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	binary := filepath.Join(dir, "dokku")
	if err := stubprog.Write(binary, "#!/bin/sh\nsleep 30\n"); err != nil {
		t.Fatalf("writing the stub: %v", err)
	}
	client := &dokku.ExecClient{Binary: binary, Timeout: 100 * time.Millisecond}

	start := time.Now()
	if _, err := client.Version(context.Background()); err == nil {
		t.Fatal("a hung invocation was reported as success")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v for a 100ms timeout", elapsed)
	}
}

// The argument vector is the contract with the sudoers rule, so both shapes of
// it are pinned here. On a real host the bare form does not work at all: the
// dokku binary re-execs itself as `sudo -u dokku -E -H "$0" "$@"` and sudo
// answers `sorry, you are not allowed to preserve the environment`, which is
// why RunAs exists. The zero value keeps the bare form because a test with a
// stub binary has no sudo to go through.
func TestDokkuIsReachedThroughSudoOnlyWhenAnAccountIsNamed(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		runAs string
		want  func(binary string) string
	}{
		"no account named, so the binary is invoked directly": {
			want: func(string) string { return "version" },
		},
		"the account the sudoers rule names": {
			runAs: dokku.DefaultRunAs,
			want: func(binary string) string {
				return "-n -u dokku " + binary + " version"
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			binary, fromDokku := stubProgram(t, "dokku", "echo 'dokku version 0.38.7'")
			sudo, fromSudo := stubProgram(t, "sudo", "exit 0")

			client := &dokku.ExecClient{Binary: binary, RunAs: tc.runAs, Sudo: sudo}
			if _, err := client.Version(context.Background()); err != nil {
				t.Fatalf("Version: %v", err)
			}

			// Whichever program dokkup actually started is the one holding the
			// vector; the other must have been left alone.
			ran, idle := fromDokku, fromSudo
			if tc.runAs != "" {
				ran, idle = fromSudo, fromDokku
			}
			if got := tc.want(binary); ran() != got {
				t.Errorf("recorded = %q, want %q", ran(), got)
			}
			if idle() != "" {
				t.Errorf("the other program ran too, with %q", idle())
			}
		})
	}
}

// The sudo hop is identical on every invocation and so tells an operator
// nothing. What a failure has to name is the Dokku command that failed.
func TestAFailureUnderSudoIsReportedAsTheDokkuCommandAndNotTheSudoOne(t *testing.T) {
	t.Parallel()

	binary, _ := stubProgram(t, "dokku", "exit 0")
	sudo, _ := stubProgram(t, "sudo", "echo 'sudo: a password is required' >&2; exit 1")

	client := &dokku.ExecClient{Binary: binary, RunAs: dokku.DefaultRunAs, Sudo: sudo}

	_, err := client.Apps(context.Background())
	if err == nil {
		t.Fatal("a refused invocation was reported as success")
	}
	if !strings.Contains(err.Error(), "dokku apps:list") {
		t.Errorf("the error does not name the command that failed: %v", err)
	}
	if strings.Contains(err.Error(), "-u dokku") {
		t.Errorf("the error reports the sudo vector rather than the dokku one: %v", err)
	}
	// Without this the operator is told a command failed and not why, and
	// "a password is required" is the whole diagnosis of a missing rule.
	if !strings.Contains(err.Error(), "sudo: a password is required") {
		t.Errorf("the error drops what sudo said: %v", err)
	}
}

func TestVersionReportsTheNumberWithoutDokkusPreamble(t *testing.T) {
	t.Parallel()

	client, recorded := stubDokku(t, "dokku version 0.38.7")

	version, err := client.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version != "0.38.7" {
		t.Errorf("version = %q, want %q", version, "0.38.7")
	}
	if got := recorded(); got != "version" {
		t.Errorf("dokku was asked %q, want %q", got, "version")
	}
}

// The exact bytes Dokku 0.38.7 prints for an app deployed and scaled to two web
// containers. The scale is in here and nowhere else machine-readable: `ps:scale`
// knows it too and answers `unknown flag: --format`.
const psReportOutput = `{"deployed":"true","processes":"2","ps-can-scale":"true",` +
	`"ps-computed-procfile-path":"Procfile","ps-computed-stop-timeout-seconds":"30",` +
	`"ps-global-procfile-path":"","ps-global-stop-timeout-seconds":"","ps-procfile-path":"",` +
	`"ps-restart-policy":"on-failure:10","ps-stop-timeout-seconds":"","restore":"true",` +
	`"running":"true","status-web.1":"running (CID: d12b5c790a7)",` +
	`"status-web.2":"running (CID: cd159f69e6a)"}`

// The same report for an app that exists and has never been deployed. There is
// no status key at all, which is the shape a new app is in for as long as it
// takes its owner to deploy it.
const psReportNotDeployedOutput = `{"deployed":"false","processes":"0","ps-can-scale":"true",` +
	`"ps-computed-procfile-path":"Procfile","ps-computed-stop-timeout-seconds":"30",` +
	`"ps-global-procfile-path":"","ps-global-stop-timeout-seconds":"","ps-procfile-path":"",` +
	`"ps-restart-policy":"on-failure:10","ps-stop-timeout-seconds":"","restore":"true",` +
	`"running":"false"}`

const domainsReportOutput = `{"app-enabled":"true",` +
	`"app-vhosts":"probe.example.com alt.example.com",` +
	`"global-enabled":"false","global-vhosts":""}`

const domainsReportNoneOutput = `{"app-enabled":"false","app-vhosts":"",` +
	`"global-enabled":"false","global-vhosts":""}`

func TestProcessTypesCountsTheContainersDokkuReportsRunning(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		output string
		want   map[string]int
	}{
		"an app scaled to two web containers": {
			output: psReportOutput,
			want:   map[string]int{"web": 2},
		},
		// Empty rather than an error: the app is there and answers, it simply
		// has nothing running, and a caller listing apps must not have to treat
		// an undeployed one as a failure.
		"an app that was never deployed": {
			output: psReportNotDeployedOutput,
			want:   map[string]int{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, _ := stubDokku(t, tc.output)

			types, err := client.ProcessTypes(context.Background(), "blog")
			if err != nil {
				t.Fatalf("ProcessTypes: %v", err)
			}
			if !maps.Equal(types, tc.want) {
				t.Errorf("process types = %v, want %v", types, tc.want)
			}
		})
	}
}

func TestDomainsReadsTheVhostsTheAppIsServedAtAndNotTheGlobalOnes(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		output string
		want   []string
	}{
		"two domains, in the order Dokku lists them": {
			output: domainsReportOutput,
			want:   []string{"probe.example.com", "alt.example.com"},
		},
		"an app served at no domain at all": {
			output: domainsReportNoneOutput,
			want:   nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, _ := stubDokku(t, tc.output)

			domains, err := client.Domains(context.Background(), "blog")
			if err != nil {
				t.Fatalf("Domains: %v", err)
			}
			if !slices.Equal(domains, tc.want) {
				t.Errorf("domains = %v, want %v", domains, tc.want)
			}
		})
	}
}

// Every flag these three commands are given, pinned. Dokku answers a flag it
// does not know with usage and exit 2, so a wrong vector here is a feature that
// never works on any host -- which is exactly how `apps:list --quiet` survived.
func TestTheAppDetailCommandsAskForNothingDokkuDoesNotUnderstand(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		call func(*dokku.ExecClient) error
		want string
	}{
		"process types come from the report, with the machine-readable flag": {
			call: func(c *dokku.ExecClient) error {
				_, err := c.ProcessTypes(context.Background(), "blog")
				return err
			},
			want: "ps:report blog --format json",
		},
		"domains come from the report, with the machine-readable flag": {
			call: func(c *dokku.ExecClient) error {
				_, err := c.Domains(context.Background(), "blog")
				return err
			},
			want: "domains:report blog --format json",
		},
		"logs with no options ask for nothing but the app": {
			call: func(c *dokku.ExecClient) error {
				return c.Logs(context.Background(), "blog", dokku.LogOptions{}, func(string) error { return nil })
			},
			want: "logs blog",
		},
		// `--tail` is Dokku's name for following, however much it reads like
		// the number of lines. That is `--num`.
		"following is --tail": {
			call: func(c *dokku.ExecClient) error {
				return c.Logs(context.Background(), "blog", dokku.LogOptions{Follow: true}, func(string) error { return nil })
			},
			want: "logs --tail blog",
		},
		"how many past lines is --num": {
			call: func(c *dokku.ExecClient) error {
				return c.Logs(context.Background(), "blog", dokku.LogOptions{Tail: 50}, func(string) error { return nil })
			},
			want: "logs --num 50 blog",
		},
		"narrowing to a process type is --ps": {
			call: func(c *dokku.ExecClient) error {
				return c.Logs(context.Background(), "blog", dokku.LogOptions{ProcessType: "worker"}, func(string) error { return nil })
			},
			want: "logs --ps worker blog",
		},
		"all of them at once, in the order Dokku documents": {
			call: func(c *dokku.ExecClient) error {
				opts := dokku.LogOptions{ProcessType: "web", Tail: 100, Follow: true}
				return c.Logs(context.Background(), "blog", opts, func(string) error { return nil })
			},
			want: "logs --tail --num 100 --ps web blog",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// Valid JSON so the report commands get as far as recording their
			// vector; the log stream reads it as one unremarkable line.
			client, recorded := stubDokku(t, "{}")

			if err := tc.call(client); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got := recorded(); got != tc.want {
				t.Errorf("dokku was asked %q, want %q", got, tc.want)
			}
		})
	}
}

// What Dokku 0.38.7 really writes: a timestamp and the container that wrote the
// line, wrapped in the colour it picked for that container.
const logsOutput = "\x1b[36m2026-08-12T12:48:51.511890541Z app[web.1]:\x1b[0m hello line 14\n" +
	"\x1b[33m2026-08-12T12:48:53.519809667Z app[web.2]:\x1b[0m hello line 15"

func TestLogLinesArriveWithoutTheColourCodesDokkuCannotBeTalkedOutOf(t *testing.T) {
	t.Parallel()

	client, _ := stubDokku(t, logsOutput)

	var lines []string
	err := client.Logs(context.Background(), "blog", dokku.LogOptions{}, func(line string) error {
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}

	want := []string{
		"2026-08-12T12:48:51.511890541Z app[web.1]: hello line 14",
		"2026-08-12T12:48:53.519809667Z app[web.2]: hello line 15",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
}

// A followed stream that delivered its lines when the command finished would be
// no stream at all: the command finishes when the operator closes the page.
func TestLogLinesArriveWhileTheCommandIsStillRunning(t *testing.T) {
	t.Parallel()

	client := stubStream(t, "#!/bin/sh\necho first\nsleep 1\necho second\n")

	start := time.Now()
	var at []time.Duration
	err := client.Logs(context.Background(), "blog", dokku.LogOptions{}, func(string) error {
		at = append(at, time.Since(start))
		return nil
	})
	ended := time.Since(start)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(at) != 2 {
		t.Fatalf("got %d lines, want 2", len(at))
	}

	// The stub writes its second line a second after its first. Buffered
	// output would land both at the same moment, and that moment is the end.
	//
	// Both assertions are against the stub's own second of sleep rather than
	// against the clock: how long a busy machine takes to fork and exec a shell
	// is not what is being measured here.
	if gap := at[1] - at[0]; gap < 500*time.Millisecond {
		t.Errorf("the two lines arrived %v apart, so they were held until the command ended", gap)
	}
	if early := ended - at[0]; early < 500*time.Millisecond {
		t.Errorf("the first line arrived %v before the command ended, so it was not delivered as it was written", early)
	}
}

// An operator who closes the page must cost the host nothing. Killing only the
// process dokkup started does not achieve that: measured on the devenv, SIGKILL
// to `dokku logs <app> -t` left seven processes behind, ending in one
// `docker logs --follow` per container, still following.
func TestCancellingAStreamLeavesNoProcessBehind(t *testing.T) {
	t.Parallel()

	pids := filepath.Join(t.TempDir(), "pids")
	client := stubStream(t, "#!/bin/sh\nsleep 300 &\n"+
		"printf '%s %s\\n' \"$$\" \"$!\" > '"+pids+"'\necho started\nwait\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancelled from inside the callback, so the stream is certainly running
	// when the caller goes away.
	err := client.Logs(ctx, "blog", dokku.LogOptions{Follow: true}, func(string) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Logs error = %v, want context.Canceled: a caller going away is not Dokku failing", err)
	}

	assertGone(t, pids)
}

var errCallerHasSeenEnough = errors.New("the caller has seen enough")

// The caller refusing a line is the third way a stream ends, and it has to stop
// the host doing the work as surely as cancelling does.
func TestACallbackThatFailsStopsTheStreamAndKillsTheProcess(t *testing.T) {
	t.Parallel()

	pids := filepath.Join(t.TempDir(), "pids")
	client := stubStream(t, "#!/bin/sh\nsleep 300 &\n"+
		"printf '%s %s\\n' \"$$\" \"$!\" > '"+pids+"'\necho started\nwait\n")

	delivered := 0
	err := client.Logs(context.Background(), "blog", dokku.LogOptions{Follow: true}, func(string) error {
		delivered++
		return errCallerHasSeenEnough
	})
	// Returned as it was given rather than wrapped in a [dokku.CommandError]:
	// the caller's own decision is not a failure of Dokku's, and the caller has
	// to be able to recognise its own sentinel.
	if !errors.Is(err, errCallerHasSeenEnough) {
		t.Fatalf("Logs error = %v, want the error the callback returned", err)
	}
	// errors.Is alone would not notice the wrapping, because CommandError
	// unwraps to what it holds. This is the assertion that fails if the
	// caller's own decision is ever dressed up as an invocation that failed.
	var commandErr *dokku.CommandError
	if errors.As(err, &commandErr) {
		t.Errorf("the caller's own decision was reported as a failure of Dokku's: %v", err)
	}
	if delivered != 1 {
		t.Errorf("the callback was called %d times after refusing the first line", delivered)
	}

	assertGone(t, pids)
}

func TestALogStreamThatDokkuRefusesIsReportedAsACommandError(t *testing.T) {
	t.Parallel()

	client := stubStream(t, "#!/bin/sh\necho ' !     App blog has not been deployed' >&2\nexit 1\n")

	err := client.Logs(context.Background(), "blog", dokku.LogOptions{}, func(string) error { return nil })

	var commandErr *dokku.CommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("Logs error = %v (%T), want a *dokku.CommandError", err, err)
	}
	if !strings.Contains(err.Error(), "has not been deployed") {
		t.Errorf("the error drops what Dokku said: %v", err)
	}
}

// A process type reaches Dokku as the value of a flag, so one beginning with a
// dash would be read as another flag by the Dokku binary itself. Nothing must
// run until it has been rejected.
func TestLogsRejectsAProcessTypeThatCouldBeReadAsAFlag(t *testing.T) {
	t.Parallel()

	client, recorded := stubDokku(t, "{}")

	opts := dokku.LogOptions{ProcessType: "--ps"}
	if err := client.Logs(context.Background(), "blog", opts, func(string) error { return nil }); err == nil {
		t.Fatal("Logs accepted a process type it must reject")
	}
	if got := recorded(); got != "" {
		t.Errorf("dokku was invoked with %q for a rejected process type", got)
	}
}

// stubStream writes a stand-in for the dokku binary from a whole script, for
// the tests about how a stream lives and dies rather than about what it says.
func stubStream(t *testing.T, script string) *dokku.ExecClient {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "dokku")
	if err := stubprog.Write(binary, script); err != nil {
		t.Fatalf("writing the stub: %v", err)
	}
	return &dokku.ExecClient{Binary: binary}
}

// assertGone reads a file of process ids the stub wrote and fails unless every
// one of them is gone. Signal 0 reports whether a process can be signalled,
// which for a process that no longer exists is ESRCH.
func assertGone(t *testing.T, path string) {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the stub's process ids: %v", err)
	}

	for _, field := range strings.Fields(string(content)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("the stub wrote %q where a process id belongs", field)
		}

		// Polled rather than read once: the kill is delivered as the stream
		// returns, and the kernel reaps in its own time.
		alive := true
		for range 100 {
			if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
				alive = false
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if alive {
			t.Errorf("process %d is still running: an abandoned stream left work behind on the host", pid)
		}
	}
}

// A line too long for the scanner is the app's doing, not Dokku's: the app
// chooses how much it writes before a newline, and one line of a megabyte is a
// JSON payload logged whole. Reading stopping must end the invocation like any
// other ending, or the stream is wedged for good -- the process goes on writing
// into a pipe nobody reads, Wait never returns, and the caller's goroutine and
// the `docker logs --follow` tree behind it are lost together.
func TestALineTooLongToReadEndsTheStreamInsteadOfWedgingIt(t *testing.T) {
	t.Parallel()

	pids := filepath.Join(t.TempDir(), "pids")
	client := stubStream(t, "#!/bin/sh\nsleep 300 &\n"+
		"printf '%s %s\\n' \"$$\" \"$!\" > '"+pids+"'\n"+
		"echo short\n"+
		// Two megabytes with no newline in them, against a scanner that gives
		// up at one.
		"head -c 2000000 /dev/zero | tr '\\0' x\n"+
		"echo\nwait\n")

	done := make(chan error, 1)
	go func() {
		done <- client.Logs(context.Background(), "blog", dokku.LogOptions{Follow: true}, func(string) error {
			return nil
		})
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		// Reported rather than waited on: the failure this guards against is
		// an invocation that never returns at all.
		t.Fatal("Logs never returned after the scanner gave up on a line")
	}

	var commandErr *dokku.CommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("Logs error = %v (%T), want a *dokku.CommandError", err, err)
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Errorf("Logs error = %v, want it to carry the scanner's own error", err)
	}

	assertGone(t, pids)
}

// A caller that was already gone before anything ran is still a caller going
// away, and not an invocation that failed. Both implementations have to say so
// the same way: a handler is written against [dokku.Client] and must not have
// to ask which of the two it is holding to know what it is being told.
func TestACallerAlreadyGoneIsNeverReportedAsAFailureOfDokkus(t *testing.T) {
	t.Parallel()

	stub, _ := stubDokku(t, logsOutput)
	fake := dokku.NewFake()
	fake.SetLogs("blog", []string{"one", "two"})

	for name, client := range map[string]dokku.Client{
		"the real client": stub,
		"the fake":        fake,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := client.Logs(ctx, "blog", dokku.LogOptions{}, func(string) error {
				t.Error("a line was delivered to a caller that had already gone away")
				return nil
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Logs error = %v, want context.Canceled", err)
			}
			// The real client returns before the switch that decides this can
			// run, because os/exec refuses to start on a dead context at all.
			var commandErr *dokku.CommandError
			if errors.As(err, &commandErr) {
				t.Errorf("a caller that had already gone away was reported as a failure of Dokku's: %v", err)
			}
		})
	}
}
