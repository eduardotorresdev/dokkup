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
func (c *ExecClient) Apps(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "apps:list", "--quiet")
	if err != nil {
		return nil, err
	}
	var apps []string
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			apps = append(apps, name)
		}
	}
	return apps, nil
}

// AppReport implements [Client].
func (c *ExecClient) AppReport(ctx context.Context, app string) (map[string]string, error) {
	if err := ValidateAppName(app); err != nil {
		return nil, err
	}
	return c.runJSON(ctx, "apps:report", app)
}
