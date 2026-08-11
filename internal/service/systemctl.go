package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultBinary is where systemd installs its control program.
const DefaultBinary = "/usr/bin/systemctl"

// defaultTimeout bounds a single invocation. A restart that has not finished in
// this long is not going to, and an updater waiting on systemd forever is worse
// than one that reports a stuck unit.
const defaultTimeout = 30 * time.Second

// waitDelay is how long Wait may go on waiting for output after the process it
// started is gone.
//
// Without it, defaultTimeout is advisory: killing the process on a context
// deadline does not close the stdout and stderr pipes, and anything that
// inherited them keeps Wait blocked. internal/dokku carries the measurement and
// the full explanation. The same bound belongs here, because the caller is
// `dokkup update` deciding whether to roll back, and an unbounded restart is
// the one thing it cannot recover from.
const waitDelay = time.Second

// Systemctl drives the local systemd through systemctl(1).
//
// Commands are argument vectors executed directly. As in internal/dokku, there
// is no shell in this file and there must never be one.
type Systemctl struct {
	// Binary is the path to systemctl. Empty means [DefaultBinary].
	Binary string

	// Unit is the unit acted on. Empty means [Name].
	Unit string

	// Timeout bounds a single invocation. Zero means [defaultTimeout].
	Timeout time.Duration
}

var _ Manager = (*Systemctl)(nil)

// NewSystemctl returns a manager for dokkup's own unit on this host.
func NewSystemctl() *Systemctl {
	return &Systemctl{Binary: DefaultBinary, Unit: Name, Timeout: defaultTimeout}
}

// CommandError describes a systemctl invocation that failed.
type CommandError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("systemctl %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
	}
	return fmt.Sprintf("systemctl %s: %v", strings.Join(e.Args, " "), e.Err)
}

func (e *CommandError) Unwrap() error { return e.Err }

// run invokes systemctl with args. Standard output is returned even when the
// command failed, because systemctl reports some answers -- notably is-active --
// through the exit status while still printing the answer.
func (s *Systemctl) run(ctx context.Context, args ...string) ([]byte, error) {
	binary := s.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = nil
	cmd.WaitDelay = waitDelay

	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &CommandError{
			Args:   args,
			Stderr: strings.TrimSpace(stderr.String()),
			Err:    err,
		}
	}
	return stdout.Bytes(), nil
}

func (s *Systemctl) unit() string {
	if s.Unit == "" {
		return Name
	}
	return s.Unit
}

// Restart implements [Manager].
func (s *Systemctl) Restart(ctx context.Context) error {
	_, err := s.run(ctx, "restart", s.unit())
	return err
}

// ResetFailed implements [Manager].
func (s *Systemctl) ResetFailed(ctx context.Context) error {
	_, err := s.run(ctx, "reset-failed", s.unit())
	return err
}

// IsActive implements [Manager].
func (s *Systemctl) IsActive(ctx context.Context) (bool, error) {
	out, err := s.run(ctx, "is-active", s.unit())
	state := strings.TrimSpace(string(out))

	if err != nil {
		// is-active answers through its exit status: non-zero means the unit is
		// not running, which is an answer rather than a failure. Only a run that
		// produced no state at all -- systemctl missing, or the context expiring
		// -- is an error worth reporting.
		var exit *exec.ExitError
		if errors.As(err, &exit) && state != "" {
			return false, nil
		}
		return false, err
	}

	return state == "active", nil
}
