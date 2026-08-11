package dokku_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
