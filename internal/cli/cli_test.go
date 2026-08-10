package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/cli"
	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
)

func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = cli.Run(cli.Env{
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

	if code == 0 {
		t.Fatal("uninstall exited 0 while unimplemented; it must not claim success")
	}
	for _, want := range []string{hostpaths.Unit, hostpaths.Sudoers, hostpaths.Binary, hostpaths.DataDir} {
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

	for _, command := range []string{"install", "uninstall", "setup-token"} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			_, stderr, code := run(t, command)
			if code != 3 {
				t.Fatalf("exit code = %d, want 3", code)
			}
			if !strings.Contains(stderr, "not implemented yet") {
				t.Errorf("stderr did not say the command is unimplemented: %q", stderr)
			}
		})
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
