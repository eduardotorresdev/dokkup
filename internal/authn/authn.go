// Package authn proves that the person at the terminal meant to do this.
//
// It exists for one command. `dokkup uninstall` is grave and irreversible for
// some of what it touches, and a mis-typed command must not be able to perform
// it. See docs/adr/0008-uninstall-removes-only-what-it-created.md.
package authn

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrNotConfirmed reports that the operator did not prove they are present and
// deliberate. Nothing has been changed when it is returned.
var ErrNotConfirmed = errors.New("not confirmed")

// waitDelay is how long Wait may go on waiting for output after the process it
// started is gone. See the comment where it is used.
const waitDelay = time.Second

// passwordPrompt is the question sudo puts to the operator. It is passed with
// -p because the environment writes it otherwise: measured on the devenv, a
// SUDO_PROMPT of "dokkup needs to check something, type your name: " was what
// `sudo -k -v` asked and it still exited 0 on the password, while the same run
// with -p asked "dokkup uninstall: password for zz-probe-auth: ". %p is sudo's
// own escape for the account whose password is wanted.
const passwordPrompt = "dokkup uninstall: password for %p: " //nolint:gosec // a prompt, not a password

// Confirmer asks one question and believes only the right answer.
type Confirmer struct {
	// In is where the answer to the host-name challenge is read from. Nil means
	// os.Stdin.
	In io.Reader

	// Out is where the host-name challenge is printed. Nil means os.Stdout. The
	// password challenge writes nothing here, because sudo owns that prompt and
	// puts it on the terminal itself.
	Out io.Writer

	// Password prompts the operator who started this command for their own
	// password and returns nil only if they got it right. Nil means there is no
	// sudo session to ask within, and the host-name challenge is asked instead.
	//
	// A sudo session with no terminal to ask on is not nil: it is a function
	// that refuses. An operator who reached for sudo asked for the strong
	// challenge, and quietly giving them the weak one would take that away
	// without saying so. [SudoPassword] builds both.
	Password func(ctx context.Context) error

	// Hostname is the name the operator must type on the host-name challenge.
	// Empty means os.Hostname.
	Hostname string
}

// Confirm returns nil only on a deliberate, correct answer.
//
// Which challenge is asked is decided by whether [Confirmer.Password] is set,
// and by nothing else. That is what makes this testable at all: the thing that
// touches the world is a function field, so both answers to both challenges
// are exercised by tests that need no terminal, no sudo session and no root,
// and the subprocess is left to sudo's own test suite.
//
// A password challenge that could not be run is not a wrong password. sudo
// missing from the PATH, or failing to start, says nothing about who is at the
// keyboard, so the host-name challenge is asked instead of the operator being
// told they typed their password wrongly.
func (c *Confirmer) Confirm(ctx context.Context) error {
	if c.Password != nil {
		switch err := c.Password(ctx); {
		case err == nil:
			return nil
		case errors.Is(err, ErrNotConfirmed):
			return err
		}
	}
	return c.confirmHostname()
}

// confirmHostname asks for this host's name and accepts it typed the way people
// type it.
//
// Case-insensitively, and trimmed, which absorbs the \r a CRLF terminal sends
// as part of the line. A hostname is not a secret and this is not guarding
// against somebody who knows it: the challenge exists so that a mis-typed
// command cannot remove anything, so rejecting DOKKUP-DEVENV buys nothing and
// costs an operator who has caps lock on. The first label is accepted as well,
// because os.Hostname() returns the whole nodename and that is an FQDN on a
// host whose nodename is one -- measured in a private UTS namespace named
// zz-probe.example.test, where os.Hostname() reported
// "zz-probe.example.test" while `hostname -s` reported zz-probe, which is what
// the operator would type.
//
// An empty answer is never a confirmation. That is also how EOF is refused: a
// closed stdin has said nothing, and `dokkup uninstall < /dev/null` must remove
// nothing rather than read it as an empty-but-valid answer.
func (c *Confirmer) confirmHostname() error {
	want := c.Hostname
	if want == "" {
		name, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("reading this host's name in order to ask for it: %w", err)
		}
		want = name
	}

	in, out := c.In, c.Out
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}

	_, _ = fmt.Fprintf(out, "This host is %s. Type its name to confirm removal: ", want)

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading the answer: %w", err)
	}

	got := strings.TrimSpace(line)
	if got == "" {
		return fmt.Errorf("%w: nothing was typed", ErrNotConfirmed)
	}
	if !strings.EqualFold(got, want) && !strings.EqualFold(got, firstLabel(want)) {
		return fmt.Errorf("%w: %q is not this host's name", ErrNotConfirmed, got)
	}
	return nil
}

func firstLabel(name string) string {
	label, _, _ := strings.Cut(name, ".")
	return label
}

// SudoPassword returns a password challenge for this process, or nil when there
// is no sudo session to ask one within.
//
// Four things are checked before there is a challenge at all, and every one of
// them is load-bearing:
//
//   - SUDO_UID parses and is not zero. Invoked as root, `sudo -k -v` exits 0
//     immediately having asked nobody anything -- measured both with no
//     terminal and under a pty -- so without this check a forged or stale
//     environment is a silent pass. Measured directly, with
//     `SUDO_USER=root SUDO_UID=0 SUDO_GID=0` in front of a probe that runs
//     exactly the command below: "probe: sudo exit=0 (authenticated)".
//
//   - SUDO_GID parses, and os/user resolves the uid to the account SUDO_USER
//     names. The environment is the operator's to write and the passwd database
//     is not, so the two disagreeing means the environment is not evidence. The
//     pure-Go reader is enough: measured in a CGO_ENABLED=0 cross-compiled
//     binary, user.LookupId("1002") returned Username="zz-probe-auth" and
//     user.LookupId("424242") returned "user: unknown userid 424242".
//
//   - sudo is on the PATH. Measured under PATH=/nonexistent, the failure is
//     `exec: "sudo": executable file not found in $PATH`, which is not an
//     *exec.ExitError and must never be reported as a wrong password.
//
//   - /dev/tty can be opened. os.Stdin's ModeCharDevice is true for /dev/null
//     as well as for a terminal, so it cannot tell one from the other --
//     measured, a probe reading stdin from /dev/null reported
//     "mode=Dcrw-rw-rw- ModeCharDevice=true" and the same probe under a pty
//     reported "mode=Dcrw--w---- ModeCharDevice=true". Opening /dev/tty does
//     tell them apart: "ok -> controlling terminal present" under the pty, and
//     "no such device or address" for the pipe and for /dev/null alike.
//
// The last of the four is the one that does not produce a nil. A sudo session
// with no terminal gets a challenge that refuses, because the alternative
// answers are both bad: downgrading to the host-name challenge takes away the
// stronger proof the operator asked for by using sudo, and running sudo anyway
// hands them sudo's own "a terminal is required to read the password; either
// use the -S option to read from standard input or configure an askpass
// helper", which is advice about a flag they never typed.
func SudoPassword() func(ctx context.Context) error {
	// ParseUint with a bit size of 32 rather than Atoi, because a uid is what
	// the Credential below takes and a number outside that range is not one.
	uid, err := strconv.ParseUint(os.Getenv("SUDO_UID"), 10, 32)
	if err != nil || uid == 0 {
		return nil
	}
	gid, err := strconv.ParseUint(os.Getenv("SUDO_GID"), 10, 32)
	if err != nil {
		return nil
	}

	// The parsed number is looked up rather than the raw variable, so that a
	// value the passwd reader and strconv would read differently cannot get
	// past both.
	invoker, err := user.LookupId(strconv.FormatUint(uid, 10))
	if err != nil || invoker.Username != os.Getenv("SUDO_USER") {
		return nil
	}

	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return nil
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return refuseWithoutTerminal
	}
	_ = tty.Close()

	return func(ctx context.Context) error {
		// One invocation, `sudo -k -v`. Used with an option that may require a
		// password, -k makes sudo ignore the cached credential, prompt, and not
		// refresh it, which is the whole of what re-authentication wants: proof
		// that the operator is present now, granting nothing afterwards.
		// Measured on the devenv with a warm ticket, `sudo -k -v` prompted and
		// exited 0, and `sudo -n -v` immediately after it answered "sudo: a
		// password is required" -- where a plain `sudo -v` had left a ticket
		// that answered 0.
		//
		// Not -K, which removes every cached credential for the user on every
		// terminal, including the one the operator's own outer
		// `sudo dokkup uninstall` is holding: rude, and proof of nothing extra.
		// Not -k and then -v either, which authenticates and leaves a fresh
		// 15-minute credential behind.
		cmd := exec.CommandContext(ctx, sudo, "-k", "-p", passwordPrompt, "-v")

		// sudo reads /dev/tty, not fd 0: measured, it authenticated with stdin
		// redirected from /dev/null as long as a controlling terminal existed.
		// Handing it a stdin it does not read would only mean an answer could
		// arrive from a pipe nobody is watching. Its output is the operator's
		// terminal for the same reason -- the prompt, "Sorry, try again." and
		// the count of attempts are sudo's to print.
		cmd.Stdin = nil
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr

		// The whole design hinges on this drop. Left as root, sudo has nothing
		// to ask and the challenge proves nothing.
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, //nolint:gosec // ParseUint bounded both to 32 bits
		}

		// Built rather than inherited, so that SUDO_PROMPT and SUDO_ASKPASS in
		// the environment cannot rewrite what the operator is asked or answer
		// it for them.
		cmd.Env = []string{
			"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
			"HOME=" + invoker.HomeDir,
			"USER=" + invoker.Username,
			"LOGNAME=" + invoker.Username,
			"TERM=" + os.Getenv("TERM"),
			"LANG=" + os.Getenv("LANG"),
		}

		// For the reason internal/dokku/exec.go sets it: a cancelled context
		// kills the process os/exec started and nothing else, and Wait goes on
		// waiting while anything that inherited its output still holds it.
		cmd.WaitDelay = waitDelay

		// No timeout around Run, deliberately. `sudo -V` on Ubuntu 24.04
		// reports "Password prompt timeout: 0.0 minutes" and "Number of tries
		// to enter a password: 3", so sudo waits as long as the person needs
		// and lets them get it wrong twice first. The /dev/tty check above is
		// what stops this running where nobody is watching; a timeout here
		// would only cut short an operator who is typing.
		if err := cmd.Run(); err != nil {
			// An ExitError is sudo having asked and been told the wrong thing.
			// The message claims no particular number of attempts: sudo allows
			// three and prints "Sorry, try again." between them, so by the time
			// dokkup has control back the operator has already been told twice.
			if errors.As(err, new(*exec.ExitError)) {
				return fmt.Errorf("%w: sudo did not accept the password", ErrNotConfirmed)
			}
			// Anything else is sudo not having run, which is not evidence about
			// the operator. [Confirmer.Confirm] asks the host-name challenge
			// instead of reporting a password nobody was asked for.
			return fmt.Errorf("running %s: %w", sudo, err)
		}
		return nil
	}
}

func refuseWithoutTerminal(context.Context) error {
	return fmt.Errorf("%w: there is no terminal to ask for a password on; "+
		"run it from a terminal, or run it as root and confirm with the host's name",
		ErrNotConfirmed)
}
