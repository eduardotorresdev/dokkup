package dokku

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultBinary is where Dokku installs its CLI.
const DefaultBinary = "/usr/bin/dokku"

// DefaultRunAs is the account Dokku is invoked as. The sudoers rule
// installation writes permits exactly this: the dokku binary, run as dokku,
// with no password. See docs/adr/0005-dedicated-system-user-not-root.md.
const DefaultRunAs = "dokku"

// DefaultSudo is where sudo lives.
const DefaultSudo = "/usr/bin/sudo"

// defaultTimeout bounds a single invocation. Dokku commands are short; one that
// runs longer than this has hung, and a hung command must not hold an HTTP
// request open indefinitely.
const defaultTimeout = 30 * time.Second

// waitDelay is how long Wait may go on waiting for output after the process it
// started is gone. It is what makes defaultTimeout enforceable rather than
// advisory, and what keeps an abandoned stream from waiting on a pipe nobody
// will close; see the comments where it is used.
const waitDelay = time.Second

// ExecClient invokes the Dokku binary on the local host.
//
// Commands are built as argument vectors and executed directly. There is no
// shell anywhere in this file, and there must never be one: it is what keeps a
// value submitted through a web form from being read as shell syntax.
type ExecClient struct {
	// Binary is the path to the dokku executable. Empty means [DefaultBinary].
	Binary string

	// RunAs is the account the Dokku binary is invoked as, through sudo. Empty
	// invokes Binary directly, which is what a test with a stub binary wants
	// and what nothing on a real host should use.
	//
	// The hop is not belt and braces on top of a working invocation; it is the
	// only form that works at all. Bare `dokku` fails for the service user even
	// with the sudoers rule and the group membership: /usr/bin/dokku re-execs
	// itself as `sudo -u dokku -E -H "$0" "$@"` and sudo answers `sorry, you
	// are not allowed to preserve the environment`. Under `-u dokku` that guard
	// is false and dokku runs directly, with no second sudo.
	//
	// Measured on the devenv (Ubuntu 24.04, Dokku 0.38.7, sudo 1.9.15p5) as a
	// throwaway account holding exactly the rule installation writes,
	// `zzsudo ALL=(dokku) NOPASSWD: /usr/bin/dokku`:
	//
	//	sudo -n -u dokku /usr/bin/dokku version
	//		exit 0, "dokku version 0.38.7"
	//	/usr/bin/dokku version
	//		exit 1, "sudo: sorry, you are not allowed to preserve the environment"
	//	sudo -n -u dokku /bin/ls /
	//		exit 1, "sudo: a password is required"
	//	sudo -n /usr/bin/dokku version
	//		exit 1, "sudo: a password is required"
	//
	// The last two are the rule doing its job rather than incidental: the
	// account may run one program as one other account and nothing else, so the
	// escalation ceiling is the dokku account rather than root.
	RunAs string

	// Sudo is the path to sudo. Empty means [DefaultSudo].
	Sudo string

	// Timeout bounds a single invocation. Zero means [defaultTimeout].
	//
	// It does not apply to [ExecClient.Logs], which has no timeout at all --
	// not even when it is not following, because there is no length of time
	// that is too long for a command whose whole job is to keep talking. What
	// ends a log stream is the caller's context, and nothing else.
	Timeout time.Duration
}

var _ Client = (*ExecClient)(nil)

// NewExecClient returns a client invoking the Dokku binary at its usual path,
// as the account the sudoers rule names.
func NewExecClient() *ExecClient {
	return &ExecClient{
		Binary:  DefaultBinary,
		RunAs:   DefaultRunAs,
		Sudo:    DefaultSudo,
		Timeout: defaultTimeout,
	}
}

// CommandError describes a Dokku invocation that failed.
type CommandError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("dokku %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
	}
	return fmt.Sprintf("dokku %s: %v", strings.Join(e.Args, " "), e.Err)
}

func (e *CommandError) Unwrap() error { return e.Err }

// command resolves the program to start and the argument vector to start it
// with, for the Dokku command args.
//
// When an account is named, the process dokkup starts is sudo and Dokku is what
// sudo starts. The two vectors are kept apart deliberately: what goes into a
// [CommandError] is args, the Dokku command, and never the sudo one. An
// operator reading a failure wants to know which Dokku command failed;
// `-n -u dokku /usr/bin/dokku` in front of it is plumbing that is identical on
// every invocation and tells them nothing.
//
// `-n` is load-bearing. Without it, a host whose sudoers rule is missing or
// wrong gets a password prompt on a service that has no terminal to answer it
// on, and the invocation hangs until the timeout instead of failing with
// `sudo: a password is required`.
func (c *ExecClient) command(args []string) (program string, vector []string) {
	binary := c.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if c.RunAs == "" {
		return binary, args
	}
	sudo := c.Sudo
	if sudo == "" {
		sudo = DefaultSudo
	}
	return sudo, append([]string{"-n", "-u", c.RunAs, binary}, args...)
}

// run invokes Dokku with args and returns its standard output.
func (c *ExecClient) run(ctx context.Context, args ...string) ([]byte, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	program, vector := c.command(args)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, program, vector...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Dokku is not interactive here, and a command that blocks reading standard
	// input would hold the request open until the timeout.
	cmd.Stdin = nil

	// Without this the timeout above is a suggestion. When the context expires,
	// os/exec kills the process it started and nothing else, but Wait does not
	// return until the stdout and stderr pipes are closed -- and every process
	// that inherited them holds them open. `dokku` is a shell script that forks
	// docker, git and plugin hooks, so there is always something else holding
	// them.
	//
	// Measured on Linux with a stub that sleeps 30s and a 100ms timeout: Run
	// returned after 30.02s without a WaitDelay and after 1.11s with one. macOS
	// returns immediately either way, which is why this was invisible until CI
	// ran the test.
	//
	// The cost is that a command whose output was still draining is reported as
	// a failure rather than waited on. That is the right way round: the caller
	// is an HTTP handler or an updater deciding whether to roll back, and both
	// would rather hear a wrong answer late than no answer at all.
	cmd.WaitDelay = waitDelay

	if err := cmd.Run(); err != nil {
		return nil, &CommandError{
			Args:   args,
			Stderr: strings.TrimSpace(stderr.String()),
			Err:    err,
		}
	}
	return stdout.Bytes(), nil
}

// runJSON invokes Dokku with --format json and decodes the result.
//
// Dokku's report commands emit a flat object of string values, so that is what
// this returns. Prefer it over parsing human-readable output wherever Dokku
// offers the flag.
func (c *ExecClient) runJSON(ctx context.Context, args ...string) (map[string]string, error) {
	out, err := c.run(ctx, append(args, "--format", "json")...)
	if err != nil {
		return nil, err
	}
	var report map[string]string
	if err := json.Unmarshal(out, &report); err != nil {
		return nil, fmt.Errorf("dokku %s: decoding json report: %w", strings.Join(args, " "), err)
	}
	return report, nil
}

// Version implements [Client].
func (c *ExecClient) Version(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "version")
	if err != nil {
		return "", err
	}
	// "dokku version 0.38.7"
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "dokku version "), nil
}

// Apps implements [Client].
//
// `apps:list` takes no flags -- Dokku 0.38.7 answers `unknown flag: --quiet` and
// exits 2 -- so the header it prints is dropped here instead. Dokku marks its own
// narration with these prefixes on every command, and only names appear without
// one, so this reads the list rather than trusting the line count.
func (c *ExecClient) Apps(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "apps:list")
	if err != nil {
		return nil, err
	}
	var apps []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || isDokkuNarration(name) {
			continue
		}
		apps = append(apps, name)
	}
	return apps, nil
}

// isDokkuNarration reports whether a line is Dokku talking to a person rather
// than answering the question.
func isDokkuNarration(line string) bool {
	for _, prefix := range []string{"=====>", "----->", "!", "NOTE:", "WARN:"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// AppReport implements [Client].
func (c *ExecClient) AppReport(ctx context.Context, app string) (map[string]string, error) {
	if err := ValidateAppName(app); err != nil {
		return nil, err
	}
	return c.runJSON(ctx, "apps:report", app)
}

// processStatus matches the report keys `ps:report` gives one running
// container, `status-<process type>.<index>`.
//
// The scale is read from these rather than from `ps:scale`, which is the other
// command that knows it, because `ps:scale` has no machine-readable form:
// Dokku 0.38.7 answers `unknown flag: --format` and prints a table with a
// `--------: ---` rule in it. `ps:report --format json` carries the same
// information as one key per container, observed on the devenv for an app
// scaled to two web containers:
//
//	{"deployed":"true","processes":"2", ... ,
//	 "status-web.1":"running (CID: d12b5c790a7)",
//	 "status-web.2":"running (CID: cd159f69e6a)"}
//
// An app that has never been deployed has no such key at all, which is why an
// app with no processes is an empty map and not an error.
var processStatus = regexp.MustCompile(`^status-(.+)\.\d+$`)

// ProcessTypes implements [Client].
func (c *ExecClient) ProcessTypes(ctx context.Context, app string) (map[string]int, error) {
	if err := ValidateAppName(app); err != nil {
		return nil, err
	}
	report, err := c.runJSON(ctx, "ps:report", app)
	if err != nil {
		return nil, err
	}

	types := make(map[string]int)
	for key := range report {
		if match := processStatus.FindStringSubmatch(key); match != nil {
			types[match[1]]++
		}
	}
	return types, nil
}

// Domains implements [Client].
//
// The app's own vhosts are the answer, and the global ones the same report
// carries are not: Dokku uses the global domain to name an app's first vhost
// when it is created, after which the app's list is what nginx is configured
// from. Observed on the devenv after two `domains:add`:
//
//	{"app-enabled":"true","app-vhosts":"probe.example.com alt.example.com",
//	 "global-enabled":"false","global-vhosts":""}
func (c *ExecClient) Domains(ctx context.Context, app string) ([]string, error) {
	if err := ValidateAppName(app); err != nil {
		return nil, err
	}
	report, err := c.runJSON(ctx, "domains:report", app)
	if err != nil {
		return nil, err
	}

	domains := strings.Fields(report["app-vhosts"])
	if len(domains) == 0 {
		return nil, nil
	}
	return domains, nil
}

// logArgs is the Dokku command opts asks for. app and any process type must
// have been validated already.
//
// Dokku's flag names read backwards from what they do, so they are spelled out
// in full: `--tail` is what keeps the stream open, and `--num` is how many past
// lines it starts with.
//
// [Fake] records what this returns rather than a shape of its own, so that a
// test asserting what was asked for is asserting the vector a real Dokku would
// have been sent.
func logArgs(app string, opts LogOptions) []string {
	args := []string{"logs"}
	if opts.Follow {
		args = append(args, "--tail")
	}
	if opts.Tail > 0 {
		args = append(args, "--num", strconv.Itoa(opts.Tail))
	}
	if opts.ProcessType != "" {
		args = append(args, "--ps", opts.ProcessType)
	}
	return append(args, app)
}

// Logs implements [Client].
func (c *ExecClient) Logs(ctx context.Context, app string, opts LogOptions, fn func(line string) error) error {
	if err := ValidateAppName(app); err != nil {
		return err
	}
	if opts.ProcessType != "" {
		if err := validateProcessType(opts.ProcessType); err != nil {
			return err
		}
	}

	return c.stream(ctx, func(line string) error {
		return fn(ansiColour.ReplaceAllString(line, ""))
	}, logArgs(app, opts)...)
}

// ansiColour matches the SGR escape sequences Dokku wraps its log prefix in.
//
// Dokku colours a log line per container without being asked to: `dokku logs`
// pipes each container's output through
// `sed -u -r 's/^([^Z]+Z )/\x1b[36m\1app[web.1]:\x1b[0m /gm'`, read out of the
// process tree of a real `dokku logs -t` on the devenv. Nothing turns it off --
// NO_COLOR, DOKKU_QUIET_OUTPUT and a stdout that is not a terminal all come
// back coloured, and no environment would survive the sudo hop anyway.
//
// The prefix itself is kept, because it says when the line was written and
// which container wrote it. Only the escape codes go, because everything that
// is not a terminal renders them as rubbish -- and dokkup's caller is a browser.
var ansiColour = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// maxLogLine bounds a single line of an app's output. Dokku prefixes each line
// and otherwise passes the app's own bytes through, so the length is the app's
// to choose, and bufio's default of 64 KiB would end the stream with
// ErrTooLong the first time an app logged a large JSON payload on one line.
const maxLogLine = 1 << 20

// maxStreamStderr caps what a stream keeps of standard error. A report's stderr
// is bounded by the command ending; a followed stream has no end, so an
// unbounded buffer would grow for as long as an operator watches the logs.
const maxStreamStderr = 4 << 10

// neverRan reports an invocation that did not get as far as running, from the
// two places that return before the precedence [ExecClient.stream] ends with.
//
// A context that was already dead is one of them: exec.CommandContext refuses
// to start on it, and the caller having gone away before anything ran is not
// Dokku failing. The distinction is not decoration -- the rest of this package
// reads a [CommandError] as "the invocation failed", which is what the tests
// use to tell a caller's own decision apart from a failure of Dokku's, and
// there is no invocation here to have failed.
func neverRan(ctx context.Context, args []string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return &CommandError{Args: args, Err: err}
}

// stream invokes Dokku with args and hands its standard output to fn one line
// at a time, as the lines arrive.
//
// It is [ExecClient.run]'s sibling and differs from it in the three ways a log
// stream needs. There is no timeout, because a followed stream is meant to run
// for as long as someone is watching. Output is not buffered, because a line an
// app wrote is of no use to the person watching once the command it came from
// has finished. And the process is killed by process group rather than on its
// own, for the reason below.
func (c *ExecClient) stream(ctx context.Context, fn func(line string) error, args ...string) error {
	program, vector := c.command(args)

	// fn refusing a line ends the stream the same way the caller going away
	// does, so both arrive at the process through one cancellation.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(streamCtx, program, vector...)

	// Dokku is not interactive here, and a command that blocks reading standard
	// input would hold the stream open with nothing coming out of it.
	cmd.Stdin = nil

	stderr := &cappedBuffer{limit: maxStreamStderr}
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return neverRan(ctx, args, err)
	}

	// Killing the process dokkup started is not enough to stop a log stream,
	// and this is the difference between an abandoned browser tab costing
	// nothing and costing a `docker logs --follow` per container until the host
	// is rebooted.
	//
	// Measured on the devenv (Dokku 0.38.7): `dokku logs <app> -t` on an app
	// scaled to two containers is a tree of eight processes -- the dokku shell
	// script, the `sudo -u dokku` it re-execs itself through, a second dokku,
	// three bash subshells and one `docker logs --follow` per container. SIGKILL
	// to the first left the other seven running, reparented to init and still
	// following. SIGKILL to the process group left none of them.
	//
	// So the child is given a process group of its own and the whole group is
	// signalled. os/exec's own cancellation kills the single process, which is
	// why Cancel is replaced rather than relied on.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	// The same backstop [ExecClient.run] needs, for the same reason: Wait does
	// not return until the stdout pipe is closed, and every process that
	// inherited it holds it open. Killing the group closes them, so this is
	// what covers the one that ignored the signal rather than the ordinary case.
	cmd.WaitDelay = waitDelay

	if err := cmd.Start(); err != nil {
		return neverRan(ctx, args, err)
	}

	var fnErr error
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(nil, maxLogLine)
	for scanner.Scan() {
		if fnErr = fn(scanner.Text()); fnErr != nil {
			cancel()
			break
		}
	}
	scanErr := scanner.Err()

	// Reading having stopped is the third way this ends, and it stops the
	// process for the same reason the other two do. Without this, a line longer
	// than [maxLogLine] wedges the caller for good: the scanner gives up, the
	// process goes on writing into a pipe nobody is reading, and Wait blocks
	// for ever -- WaitDelay only counts once the process has exited or the
	// context has been cancelled, and neither has happened. The line length is
	// the deployed app's to choose, so that would be an app able to hang the
	// handler that follows its logs by writing one long line.
	if scanErr != nil {
		cancel()
	}

	waitErr := cmd.Wait()

	switch {
	// fn's error is returned as it was given, so a caller can recognise its own
	// sentinel with errors.Is. Wrapping it in a [CommandError] would report the
	// caller's decision as a failure of Dokku's.
	case fnErr != nil:
		return fnErr

	// A command that ran to completion is a success even if the caller went
	// away as the last line came out; there is nothing left to cancel.
	case waitErr == nil && scanErr == nil:
		return nil

	// A caller cancelling is not Dokku failing, but the two have to be told
	// apart, so the context's own error is what comes back.
	case ctx.Err() != nil:
		return ctx.Err()

	case scanErr != nil:
		return &CommandError{Args: args, Stderr: stderr.String(), Err: scanErr}

	default:
		return &CommandError{Args: args, Stderr: stderr.String(), Err: waitErr}
	}
}

// cappedBuffer keeps the last limit bytes written to it and discards what came
// before.
//
// The last rather than the first, because of the case the cap exists for: a
// stream followed for hours, ending in a failure. Keeping the head would hand
// the operator four kilobytes of warnings from when they opened the page and
// throw away the message explaining why it just stopped, which is the only line
// they need. Nothing says the output was truncated, and nothing needs to: what
// this is read for is [CommandError.Stderr], a diagnosis of how the command
// ended.
type cappedBuffer struct {
	limit int
	buf   bytes.Buffer
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.buf.Write(p)
	if excess := b.buf.Len() - b.limit; excess > 0 {
		b.buf.Next(excess)
	}
	// The writer is the process's own stderr, and telling it its output was
	// short-written would make it fail on a truncation only dokkup cares about.
	return len(p), nil
}

func (b *cappedBuffer) String() string { return strings.TrimSpace(b.buf.String()) }
