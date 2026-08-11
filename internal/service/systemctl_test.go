package service_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/service"
)

// stubSystemctl writes a stand-in for systemctl that records the argument
// vector it was given and then behaves as body says. Testing the seam this way
// covers what actually matters -- the arguments, and how an exit status is read
// -- on a machine with no systemd on it.
func stubSystemctl(t *testing.T, body string) (*service.Systemctl, func() string) {
	t.Helper()

	dir := t.TempDir()
	binary := filepath.Join(dir, "systemctl")
	log := filepath.Join(dir, "args")

	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\n" + body + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the stub: %v", err)
	}

	recorded := func() string {
		content, err := os.ReadFile(log)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(content))
	}

	return &service.Systemctl{Binary: binary, Unit: "dokkup.service"}, recorded
}

func TestRestartAsksSystemdForExactlyOneUnit(t *testing.T) {
	t.Parallel()

	systemctl, recorded := stubSystemctl(t, "exit 0")

	if err := systemctl.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if got := recorded(); got != "restart dokkup.service" {
		t.Errorf("systemctl was asked %q, want %q", got, "restart dokkup.service")
	}
}

func TestARestartThatSystemdRefusesIsReportedWithWhatItSaid(t *testing.T) {
	t.Parallel()

	systemctl, _ := stubSystemctl(t, "echo 'Job for dokkup.service failed' >&2; exit 1")

	err := systemctl.Restart(context.Background())
	if err == nil {
		t.Fatal("a failed restart was reported as success")
	}
	// The operator has to be told why, and systemd only says it on stderr.
	if !strings.Contains(err.Error(), "Job for dokkup.service failed") {
		t.Errorf("the error drops what systemd said: %v", err)
	}
}

// A rollback restores a working binary onto a host whose unit systemd has
// already given up on. Without clearing that first, the restore is refused and
// a recoverable host is reported as broken.
func TestAFailedUnitIsClearedByItsOwnCommandRatherThanByRestarting(t *testing.T) {
	t.Parallel()

	systemctl, recorded := stubSystemctl(t, "exit 0")

	if err := systemctl.ResetFailed(context.Background()); err != nil {
		t.Fatalf("ResetFailed: %v", err)
	}
	if got := recorded(); got != "reset-failed dokkup.service" {
		t.Errorf("systemctl was asked %q, want %q", got, "reset-failed dokkup.service")
	}
}

// `systemctl is-active` answers through its exit status. Treating a non-zero
// exit as a failed invocation would make every stopped service look like a
// broken systemd, and an updater would report the wrong thing after a rollback.
func TestIsActiveReadsTheStateRatherThanTheExitStatus(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		body   string
		want   bool
		fails  bool
		reason string
	}{
		"running": {
			body: "echo active; exit 0",
			want: true,
		},
		"stopped": {
			body: "echo inactive; exit 3",
			want: false,
		},
		"crashed": {
			body: "echo failed; exit 3",
			want: false,
		},
		"still coming up": {
			body: "echo activating; exit 3",
			want: false,
		},
		"systemctl itself could not answer": {
			body:   "exit 127",
			fails:  true,
			reason: "no state printed at all means the question never got asked",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			systemctl, _ := stubSystemctl(t, tc.body)

			active, err := systemctl.IsActive(context.Background())
			if tc.fails {
				if err == nil {
					t.Fatalf("expected an error: %s", tc.reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("IsActive: %v", err)
			}
			if active != tc.want {
				t.Errorf("active = %v, want %v", active, tc.want)
			}
		})
	}
}

// A hung systemctl must not hold an update open forever, because the operator
// is waiting on it and the service is down while they do.
func TestAnInvocationThatHangsIsGivenUpOn(t *testing.T) {
	t.Parallel()

	// This one only bites on Linux, which is the platform that matters: killing
	// the shell there leaves the sleep holding the stdout pipe, and Wait goes on
	// waiting for it. On macOS the pipes close as the process dies, so the test
	// passes whether or not the bound it is checking exists. It was written on
	// macOS, passed there, and failed the moment CI ran it -- measured at 30s
	// for a 100ms timeout.
	systemctl, _ := stubSystemctl(t, "sleep 30")
	systemctl.Timeout = 100 * time.Millisecond

	start := time.Now()
	if err := systemctl.Restart(context.Background()); err == nil {
		t.Fatal("a hung invocation was reported as success")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v for a 100ms timeout", elapsed)
	}
}
