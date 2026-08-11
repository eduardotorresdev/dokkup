package authn_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/authn"
)

func TestTheHostNameIsAcceptedWhicheverWayItIsTyped(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		host  string
		typed string
	}{
		"exactly as it was printed": {host: "dokkup-devenv", typed: "dokkup-devenv\n"},

		// A CRLF terminal sends the carriage return as part of the line, so
		// without the trim the answer that looks right on screen is rejected.
		"with the carriage return a CRLF terminal sends": {
			host:  "dokkup-devenv",
			typed: "dokkup-devenv\r\n",
		},

		"with whitespace around it": {host: "dokkup-devenv", typed: "  dokkup-devenv \n"},

		// The challenge is against a mis-typed command, and caps lock is not a
		// mis-typed command.
		"in capitals": {host: "dokkup-devenv", typed: "DOKKUP-DEVENV\n"},

		// os.Hostname() reports the whole nodename, which is an FQDN on a host
		// whose nodename is one, and operators know the box by its first label.
		"as the first label of an FQDN": {host: "zz-probe.example.test", typed: "zz-probe\n"},
		"as the whole FQDN":             {host: "zz-probe.example.test", typed: "zz-probe.example.test\n"},

		// `printf 'the-host' | dokkup uninstall` is a line all the same: the
		// answer arrives together with the EOF rather than before it.
		"without a trailing newline": {host: "dokkup-devenv", typed: "dokkup-devenv"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var out strings.Builder
			confirmer := &authn.Confirmer{
				In:       strings.NewReader(tc.typed),
				Out:      &out,
				Hostname: tc.host,
			}

			if err := confirmer.Confirm(t.Context()); err != nil {
				t.Errorf("confirm = %v, want nil: %q is how an operator types %q", err, tc.typed, tc.host)
			}
			if !strings.Contains(out.String(), tc.host) {
				t.Errorf("the operator was never told which host this is: %q", out.String())
			}
		})
	}
}

func TestAnythingElseIsARefusalIncludingSayingNothing(t *testing.T) {
	t.Parallel()

	for name, typed := range map[string]string{
		"another host's name": "not-this-host\n",
		"a near miss":         "dokkup-deven\n",

		// A closed stdin has said nothing. `dokkup uninstall < /dev/null` must
		// remove nothing rather than read EOF as an empty-but-valid answer.
		"nothing at all, then EOF": "",
		"a bare newline":           "\n",
		"only whitespace":          "   \t\n",

		// The first label is accepted for this host, not for any host.
		"the first label of some other host": "zz-probe\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var out strings.Builder
			confirmer := &authn.Confirmer{
				In:       strings.NewReader(typed),
				Out:      &out,
				Hostname: "dokkup-devenv",
			}

			if err := confirmer.Confirm(t.Context()); !errors.Is(err, authn.ErrNotConfirmed) {
				t.Errorf("confirm = %v, want ErrNotConfirmed: %q would have removed this installation", err, typed)
			}
		})
	}
}

// The guard this asserts is the one the whole password branch rests on. Invoked
// as root, `sudo -k -v` exits 0 immediately having asked nobody anything --
// measured with no terminal and again under a pty -- so an environment that
// merely claims a sudo session would otherwise be a silent pass.
func TestRunningAsRootIsNotProofOfAnything(t *testing.T) {
	// Not parallel, here or in the subtests: they set SUDO_UID and SUDO_USER,
	// and t.Setenv forbids the two together.
	for name, tc := range map[string]struct {
		uid  string
		gid  string
		user string
	}{
		"the environment sudo writes when root ran sudo": {uid: "0", gid: "0", user: "root"},

		// The ordinary case on a Dokku host: someone logged in as root and
		// typed the command.
		"no sudo session at all": {},

		"a SUDO_UID that is not a number":        {uid: "root", gid: "0", user: "root"},
		"a SUDO_UID with a sign in front of it":  {uid: "+1002", gid: "1002", user: "nobody"},
		"a uid this host does not have":          {uid: "424242", gid: "424242", user: "nobody"},
		"a uid that resolves to somebody else":   {uid: "1", gid: "1", user: "zz-not-this-account"},
		"a SUDO_GID that is not a number either": {uid: "1002", gid: "root", user: "root"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SUDO_UID", tc.uid)
			t.Setenv("SUDO_GID", tc.gid)
			t.Setenv("SUDO_USER", tc.user)

			if authn.SudoPassword() != nil {
				t.Error("this counted as a sudo session, so a forged environment is a silent pass")
			}
		})
	}

	// Having no password challenge is not a refusal: it is what makes the
	// host-name challenge the question. It is also the only question root can
	// be asked, because on a Dokku host root's shadow field is `*` -- there is
	// no password to check even in principle.
	t.Setenv("SUDO_UID", "0")
	t.Setenv("SUDO_GID", "0")
	t.Setenv("SUDO_USER", "root")

	var out strings.Builder
	confirmer := &authn.Confirmer{
		In:       strings.NewReader("dokkup-devenv\n"),
		Out:      &out,
		Password: authn.SudoPassword(),
		Hostname: "dokkup-devenv",
	}

	if err := confirmer.Confirm(t.Context()); err != nil {
		t.Errorf("confirm = %v, want nil: root is asked for the host's name", err)
	}
	if !strings.Contains(out.String(), "dokkup-devenv") {
		t.Errorf("root was never asked anything: %q", out.String())
	}
}

// The operator who reached for sudo asked for the strong challenge. Answering
// them with the weak one because there is no terminal would take that away
// without saying so, and letting sudo answer instead produces "either use the
// -S option", advice about a flag they never typed. This is the branch that
// keeps [authn.SudoPassword]'s third answer -- a challenge that refuses --
// distinguishable from its nil.
func TestASudoSessionWithNoTerminalIsRefusedRatherThanDowngraded(t *testing.T) {
	t.Parallel()

	refuses := func(context.Context) error {
		return fmt.Errorf("%w: there is no terminal to ask for a password on", authn.ErrNotConfirmed)
	}

	var out strings.Builder
	confirmer := &authn.Confirmer{
		// The right answer to the question that must not be asked.
		In:       strings.NewReader("dokkup-devenv\n"),
		Out:      &out,
		Password: refuses,
		Hostname: "dokkup-devenv",
	}

	if err := confirmer.Confirm(t.Context()); !errors.Is(err, authn.ErrNotConfirmed) {
		t.Fatalf("confirm = %v, want ErrNotConfirmed", err)
	}
	if out.Len() != 0 {
		t.Errorf("the host-name challenge was asked anyway: %q", out.String())
	}
}

// Told apart because sudo failing to run says nothing about who is at the
// keyboard: reporting it as a wrong password would accuse an operator who was
// never asked, and refusing outright would strand a host where sudo is missing.
func TestAPasswordChallengeThatCouldNotBeRunIsNotAWrongPassword(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	confirmer := &authn.Confirmer{
		In:  strings.NewReader("dokkup-devenv\n"),
		Out: &out,
		Password: func(context.Context) error {
			// What exec reports under PATH=/nonexistent, measured.
			return errors.New(`exec: "sudo": executable file not found in $PATH`)
		},
		Hostname: "dokkup-devenv",
	}

	if err := confirmer.Confirm(t.Context()); err != nil {
		t.Errorf("confirm = %v, want nil: the host-name challenge was never asked", err)
	}
	if !strings.Contains(out.String(), "dokkup-devenv") {
		t.Errorf("nothing was asked at all: %q", out.String())
	}
}
