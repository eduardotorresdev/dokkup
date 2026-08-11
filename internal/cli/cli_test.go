package cli_test

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/cli"
	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
)

func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = cli.Run(cli.Env{
		// Empty rather than nil, so that a command which asks a question here
		// reads end-of-input and refuses, instead of falling through to the
		// real os.Stdin and hanging the suite.
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errOut,
		Build:  cli.Build{Version: "v0.0.0-test", Commit: "abc1234", Date: "2026-01-01T00:00:00Z"},
	}, args)
	return out.String(), errOut.String(), code
}

func TestVersionPrintsTheBuildAndSucceeds(t *testing.T) {
	t.Parallel()

	stdout, _, code := run(t, "version")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "v0.0.0-test") || !strings.Contains(stdout, "abc1234") {
		t.Errorf("version output missing build details: %q", stdout)
	}
}

func TestUnknownCommandFailsWithUsage(t *testing.T) {
	t.Parallel()

	_, stderr, code := run(t, "frobnicate")

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Errorf("stderr did not name the problem: %q", stderr)
	}
}

func TestNoArgumentsFailsRatherThanDoingSomething(t *testing.T) {
	t.Parallel()

	if _, _, code := run(t); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// Removal is irreversible for some of what it touches, so the report is part of
// the command rather than something an operator must know to ask for.
func TestUninstallAlwaysReportsBeforeDoingAnything(t *testing.T) {
	t.Parallel()

	stdout, _, code := run(t, "uninstall")

	// Non-zero whatever this suite is running on: an unprivileged host refuses
	// before the challenge, and a privileged one reaches the challenge and gets
	// an empty answer from the reader above. Neither removes anything, and both
	// print the report first.
	if code == 0 {
		t.Fatal("uninstall exited 0 without an answer to its challenge")
	}
	for _, want := range []string{
		hostpaths.Unit, hostpaths.Sudoers, hostpaths.Binary, hostpaths.DataDir,
		// Installation does not write this one, so it is absent from
		// hostpaths.Owned and easy to forget here. `dokkup update` leaves it
		// behind, and a host that was ever updated would otherwise keep an
		// executable dokkup put there after being told dokkup was gone.
		hostpaths.PreviousBinary,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("removal report does not mention %s", want)
		}
	}
}

// The promise that removal will not break the server is only useful if it is
// stated where an operator will read it.
func TestUninstallReportNamesWhatItLeavesAlone(t *testing.T) {
	t.Parallel()

	stdout, _, _ := run(t, "uninstall")

	for _, want := range []string{
		hostpaths.DokkuStorageRoot,
		"Dokku plugins",
		"registry credentials",
		"NOT touch",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("removal report does not state that it leaves %q alone", want)
		}
	}
}

func TestUninstallWithPurgeSaysTheDataDirectoryGoesWithoutAsking(t *testing.T) {
	t.Parallel()

	stdout, _, _ := run(t, "uninstall", "--purge")

	if !strings.Contains(stdout, "--purge given") {
		t.Errorf("purge report does not distinguish itself from the default: %q", stdout)
	}
	if strings.Contains(stdout, "will ASK before removing") {
		t.Error("purge report still claims it will ask")
	}
}

func TestUnimplementedCommandsExitDistinctlyRatherThanLookingLikeSuccess(t *testing.T) {
	t.Parallel()

	// install and uninstall have left this list: they do their job now. The
	// setup token has not, because the store it needs does not exist yet
	// (#13, #14), and publishing has not either; leaving them here is how the
	// suite records that both are still owed.
	for name, args := range map[string][]string{
		"setup-token": {"setup-token"},
		"publish":     {"publish", "dokkup.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, stderr, code := run(t, args...)
			if code != 3 {
				t.Fatalf("exit code = %d, want 3", code)
			}
			if !strings.Contains(stderr, "not implemented yet") {
				t.Errorf("stderr did not say the command is unimplemented: %q", stderr)
			}
		})
	}
}

// Installation is refused before it changes anything, and the plan is printed
// before the refusal.
//
// Which refusal comes back depends on where this runs -- on a developer's macOS
// it is the platform check, and on an unprivileged Linux host it is the root
// check -- and the property worth pinning is the one they share: someone who
// runs `dokkup install` to find out what it would do gets the answer as well as
// the refusal, and nothing on this machine is touched to produce it.
func TestInstallRefusesBeforeChangingAnythingAndStillPrintsThePlan(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		t.Skip("as root on Linux this would attempt a real installation of this host")
	}

	stdout, stderr, code := run(t, "install")

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout, "dokkup install will:") {
		t.Errorf("the plan was not printed before the refusal: %q", stdout)
	}
	for _, want := range []string{hostpaths.Unit, hostpaths.Sudoers, hostpaths.Binary} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the install plan does not mention %s", want)
		}
	}
	if !strings.Contains(stderr, "dokkup install:") {
		t.Errorf("stderr did not name the command that refused: %q", stderr)
	}
}

func TestPublishRequiresADomain(t *testing.T) {
	t.Parallel()

	_, stderr, code := run(t, "publish")

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "usage:") {
		t.Errorf("stderr did not show usage: %q", stderr)
	}
}
