package dokku

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultBinary is where Dokku installs its CLI.
const DefaultBinary = "/usr/bin/dokku"

// defaultTimeout bounds a single invocation. Dokku commands are short; one that
// runs longer than this has hung, and a hung command must not hold an HTTP
// request open indefinitely.
const defaultTimeout = 30 * time.Second

// waitDelay is how long Wait may go on waiting for output after the process it
// started is gone. It is what makes defaultTimeout enforceable rather than
// advisory; see the comment where it is used.
const waitDelay = time.Second

// ExecClient invokes the Dokku binary on the local host.
//
// Commands are built as argument vectors and executed directly. There is no
// shell anywhere in this file, and there must never be one: it is what keeps a
// value submitted through a web form from being read as shell syntax.
type ExecClient struct {
	// Binary is the path to the dokku executable. Empty means [DefaultBinary].
	Binary string

	// Timeout bounds a single invocation. Zero means [defaultTimeout].
	Timeout time.Duration
}

var _ Client = (*ExecClient)(nil)

// NewExecClient returns a client invoking the Dokku binary at its usual path.
func NewExecClient() *ExecClient {
	return &ExecClient{Binary: DefaultBinary, Timeout: defaultTimeout}
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

// run invokes Dokku with args and returns its standard output.
func (c *ExecClient) run(ctx context.Context, args ...string) ([]byte, error) {
	binary := c.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, args...)
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
