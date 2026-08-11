package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/release"
	"github.com/eduardotorresdev/dokkup/internal/server"
	"github.com/eduardotorresdev/dokkup/internal/service"
)

// fakeSource stands in for GitHub. What it delivers is a file whose contents are
// the version the installed binary would report, which is what lets the health
// probe below be honest: it reads the binary that is actually in place.
type fakeSource struct {
	published release.Release
	delivered string
	err       error
}

func (f *fakeSource) Latest(context.Context) (release.Release, error) {
	return f.published, f.err
}

func (f *fakeSource) Resolve(_ context.Context, tag string) (release.Release, error) {
	if f.err != nil {
		return release.Release{}, f.err
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	if tag != f.published.Tag {
		return release.Release{}, fmt.Errorf("%w: %s", release.ErrNoRelease, tag)
	}
	return f.published, nil
}

func (f *fakeSource) Fetch(_ context.Context, _ release.Release, _, dir string) (release.Artifact, error) {
	if f.err != nil {
		return release.Artifact{}, f.err
	}
	file, err := os.CreateTemp(dir, release.TempPrefix+"*")
	if err != nil {
		return release.Artifact{}, err
	}
	defer func() { _ = file.Close() }()

	if _, err := file.WriteString(f.delivered); err != nil {
		return release.Artifact{}, err
	}
	return release.Artifact{Path: file.Name(), Name: "dokkup_linux_amd64", Signed: true}, nil
}

// harness is a host with dokkup installed: a binary, a service manager, and a
// health endpoint that reports whichever binary is currently in place.
type harness struct {
	t       *testing.T
	dir     string
	binary  string
	manager *service.Fake
	up      *updater
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer

	// healthErr, when set, is what the endpoint returns instead of a version.
	healthErr error
}

// newHost installs `running`, publishes `published`, and arranges for the
// download to deliver a binary that reports `delivered` -- which is how a
// version that does not come up is modelled without needing a broken program.
func newHost(t *testing.T, running, published, delivered string) *harness {
	t.Helper()

	dir := t.TempDir()
	binary := filepath.Join(dir, "dokkup")
	if err := os.WriteFile(binary, []byte(running), 0o755); err != nil {
		t.Fatalf("installing the running binary: %v", err)
	}

	h := &harness{
		t:       t,
		dir:     dir,
		binary:  binary,
		manager: &service.Fake{Active: true},
		stdout:  &bytes.Buffer{},
		stderr:  &bytes.Buffer{},
	}

	h.up = &updater{
		env:     Env{Stdout: h.stdout, Stderr: h.stderr},
		source:  &fakeSource{published: release.Release{Tag: "v" + published}, delivered: delivered},
		manager: h.manager,
		binary:  binary,
		goarch:  "amd64",
		running: running,
		// Short enough that a rollback test is not a pause, long enough that a
		// loaded machine still gets several attempts.
		timeout:  300 * time.Millisecond,
		interval: 5 * time.Millisecond,
	}

	// The endpoint reports whichever binary is installed right now, so the gate
	// is tested against the file the swap and the rollback actually move.
	h.up.health = func(context.Context) (string, error) {
		if h.healthErr != nil {
			return "", h.healthErr
		}
		content, err := os.ReadFile(binary)
		if err != nil {
			return "", err
		}
		return string(content), nil
	}

	return h
}

func (h *harness) installed() string {
	h.t.Helper()

	content, err := os.ReadFile(h.binary)
	if err != nil {
		h.t.Fatalf("reading the installed binary: %v", err)
	}
	return string(content)
}

func (h *harness) leftover() []string {
	h.t.Helper()

	found, err := os.ReadDir(h.dir)
	if err != nil {
		h.t.Fatalf("reading %s: %v", h.dir, err)
	}
	var names []string
	for _, entry := range found {
		if strings.HasPrefix(entry.Name(), release.TempPrefix) {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestAnUpdateThatComesUpHealthyIsInstalledAndTheOldBinaryIsKept(t *testing.T) {
	t.Parallel()

	host := newHost(t, "0.1.0", "0.2.0", "0.2.0")

	if err := host.up.update(context.Background(), "", false); err != nil {
		t.Fatalf("update: %v", err)
	}

	if got := host.installed(); got != "0.2.0" {
		t.Errorf("installed = %q, want 0.2.0", got)
	}
	if host.manager.Restarts() != 1 {
		t.Errorf("restarts = %d, want 1", host.manager.Restarts())
	}

	// Keeping the previous version is what makes the next update's rollback
	// possible without another download.
	previous, err := os.ReadFile(host.binary + hostpaths.PreviousSuffix)
	if err != nil {
		t.Fatalf("the previous binary was not kept: %v", err)
	}
	if string(previous) != "0.1.0" {
		t.Errorf("previous = %q, want 0.1.0", previous)
	}
	if left := host.leftover(); len(left) != 0 {
		t.Errorf("a finished update left %v beside the binary", left)
	}
}

// The test the whole rollback path exists for. A version that installs but does
// not come back healthy must leave the host running what it was running before,
// and must say so rather than reporting success.
func TestAVersionThatDoesNotComeUpIsRolledBackToTheOneThatWasWorking(t *testing.T) {
	t.Parallel()

	host := newHost(t, "0.1.0", "0.2.0", "a binary that never answers as 0.2.0")

	err := host.up.update(context.Background(), "", false)

	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("error = %v, want it to wrap ErrRolledBack", err)
	}
	if got := host.installed(); got != "0.1.0" {
		t.Errorf("installed = %q, want the previous 0.1.0 back", got)
	}
	// Once for the version that failed, once for the one put back. A rollback
	// that forgot the second restart would leave the old binary on disk and the
	// new one still running.
	if host.manager.Restarts() != 2 {
		t.Errorf("restarts = %d, want 2", host.manager.Restarts())
	}
	if exitCode(err) != 4 {
		t.Errorf("exit code = %d, want 4", exitCode(err))
	}
	// The version being undone is the one that crash-looped, so systemd has
	// very likely stopped accepting start requests for it. Restoring without
	// clearing that first would be refused.
	if !slices.Contains(host.manager.Calls, "reset-failed") {
		t.Errorf("the rollback did not clear the failed unit: %v", host.manager.Calls)
	}
	if !strings.Contains(host.stderr.String(), "previous binary back") {
		t.Errorf("the operator was not told a rollback happened: %q", host.stderr.String())
	}
}

// The worse outcome, and the one that must not be confused with the better one.
func TestARollbackThatDoesNotRestoreAWorkingServiceIsReportedDifferently(t *testing.T) {
	t.Parallel()

	host := newHost(t, "0.1.0", "0.2.0", "0.2.0")
	host.healthErr = errors.New("connection refused")

	err := host.up.update(context.Background(), "", false)

	if !errors.Is(err, ErrRollbackFailed) {
		t.Fatalf("error = %v, want it to wrap ErrRollbackFailed", err)
	}
	if errors.Is(err, ErrRolledBack) {
		t.Error("a failed rollback was also reported as a successful one")
	}
	if exitCode(err) != 5 {
		t.Errorf("exit code = %d, want 5", exitCode(err))
	}
	// Even when nothing answers, the binary that was working is what should be
	// on disk for whoever comes to fix it.
	if got := host.installed(); got != "0.1.0" {
		t.Errorf("installed = %q, want the previous 0.1.0 back", got)
	}
}

func TestADownloadThatCannotBeVerifiedNeverReachesTheBinaryOrTheService(t *testing.T) {
	t.Parallel()

	host := newHost(t, "0.1.0", "0.2.0", "0.2.0")
	host.up.source = &fakeSource{
		published: release.Release{Tag: "v0.2.0"},
		err:       errors.New("checksum mismatch for dokkup_linux_amd64"),
	}

	err := host.up.update(context.Background(), "", false)

	if err == nil {
		t.Fatal("an unverifiable download was installed")
	}
	if got := host.installed(); got != "0.1.0" {
		t.Errorf("installed = %q, want the running 0.1.0 untouched", got)
	}
	if host.manager.Restarts() != 0 {
		t.Errorf("restarts = %d, want 0: the service was restarted for a download that never verified", host.manager.Restarts())
	}
	if _, err := os.Stat(host.binary + hostpaths.PreviousSuffix); err == nil {
		t.Error("a backup was taken although nothing was installed")
	}
}

func TestUpdatingToTheVersionAlreadyRunningDoesNothingAtAll(t *testing.T) {
	t.Parallel()

	host := newHost(t, "0.2.0", "0.2.0", "0.2.0")

	if err := host.up.update(context.Background(), "", false); err != nil {
		t.Fatalf("update: %v", err)
	}
	if host.manager.Restarts() != 0 {
		t.Error("the service was restarted to install the version already running")
	}
	if !strings.Contains(host.stdout.String(), "already installed") {
		t.Errorf("output did not say it was already up to date: %q", host.stdout.String())
	}
}

// Going backwards is sometimes exactly what an operator wants, and is never
// what they want by accident.
func TestGoingBackToAnOlderVersionHasToBeAskedForExplicitly(t *testing.T) {
	t.Parallel()

	host := newHost(t, "0.2.0", "0.1.0", "0.1.0")

	err := host.up.update(context.Background(), "", false)
	if err == nil {
		t.Fatal("a downgrade happened without being asked for")
	}
	if !strings.Contains(err.Error(), "--allow-downgrade") {
		t.Errorf("the error does not say how to do it deliberately: %v", err)
	}
	if got := host.installed(); got != "0.2.0" {
		t.Errorf("installed = %q, want 0.2.0 untouched", got)
	}

	if err := host.up.update(context.Background(), "", true); err != nil {
		t.Fatalf("update --allow-downgrade: %v", err)
	}
	if got := host.installed(); got != "0.1.0" {
		t.Errorf("installed = %q, want the requested 0.1.0", got)
	}
}

// A build from a working tree has no place on the version line, so "is the
// release newer" is unanswerable rather than false.
func TestADevelopmentBuildIsReplacedRatherThanComparedAgainst(t *testing.T) {
	t.Parallel()

	host := newHost(t, "dev", "0.1.0", "0.1.0")

	if err := host.up.update(context.Background(), "", false); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := host.installed(); got != "0.1.0" {
		t.Errorf("installed = %q, want 0.1.0", got)
	}
	if !strings.Contains(host.stdout.String(), "development build") {
		t.Errorf("output did not mention what was replaced: %q", host.stdout.String())
	}
}

func TestAPinnedVersionThatWasNeverPublishedFailsWithoutTouchingAnything(t *testing.T) {
	t.Parallel()

	host := newHost(t, "0.1.0", "0.2.0", "0.2.0")

	err := host.up.update(context.Background(), "v9.9.9", false)
	if !errors.Is(err, release.ErrNoRelease) {
		t.Fatalf("error = %v, want it to wrap ErrNoRelease", err)
	}
	if got := host.installed(); got != "0.1.0" {
		t.Errorf("installed = %q, want 0.1.0 untouched", got)
	}
}

// --check runs unattended, so it must change nothing and must not turn "an
// update is waiting" into a failure a cron would page someone about.
func TestCheckReportsAndChangesNothing(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		running   string
		published string
		says      string
	}{
		"an update is waiting": {running: "0.1.0", published: "0.2.0", says: "0.2.0 is available"},
		"already up to date":   {running: "0.2.0", published: "0.2.0", says: "newest release"},
		"a development build":  {running: "dev", published: "0.2.0", says: "development build"},
		"ahead of the release": {running: "0.3.0", published: "0.2.0", says: "newer than"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			host := newHost(t, tc.running, tc.published, tc.published)

			if err := host.up.check(context.Background(), ""); err != nil {
				t.Fatalf("check: %v", err)
			}
			if got := host.stdout.String(); !strings.Contains(got, tc.says) {
				t.Errorf("output = %q, want it to mention %q", got, tc.says)
			}
			if got := host.installed(); got != tc.running {
				t.Errorf("installed = %q, want %q untouched", got, tc.running)
			}
			if len(host.manager.Calls) != 0 {
				t.Errorf("--check touched the service: %v", host.manager.Calls)
			}
		})
	}
}

func TestACheckThatCannotBeMadeIsAFailureRatherThanSilence(t *testing.T) {
	t.Parallel()

	host := newHost(t, "0.1.0", "0.2.0", "0.2.0")
	host.up.source = &fakeSource{err: errors.New("no route to host")}

	err := host.up.check(context.Background(), "")
	if err == nil {
		t.Fatal("a check that never reached GitHub reported success")
	}
	if exitCode(err) != 1 {
		t.Errorf("exit code = %d, want 1: a cron has to tell this from being up to date", exitCode(err))
	}
}

// The unit is the authority on how the service runs, so it is where the address
// comes from. Guessing instead would poll a port nothing is listening on and
// roll back a perfectly good update.
func TestTheHealthEndpointComesOutOfTheInstalledUnit(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		unit string
		want string
	}{
		"the address the unit sets": {
			unit: service.UnitFile(service.UnitConfig{Listen: "127.0.0.1:9999"}),
			want: "http://127.0.0.1:9999/api/health",
		},
		"the default when the unit says nothing": {
			unit: "[Service]\nExecStart=/usr/local/bin/dokkup serve\n",
			want: "http://" + service.DefaultListen + "/api/health",
		},
		// A wildcard is an address to listen on, not one to connect to.
		"a wildcard bind is polled on loopback": {
			unit: "[Service]\nExecStart=/usr/local/bin/dokkup serve --listen 0.0.0.0:8080\n",
			want: "http://127.0.0.1:8080/api/health",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "dokkup.service")
			if err := os.WriteFile(path, []byte(tc.unit), 0o600); err != nil {
				t.Fatalf("writing the unit: %v", err)
			}
			if got := healthURLFromUnit(path); got != tc.want {
				t.Errorf("health URL = %q, want %q", got, tc.want)
			}
		})
	}

	// No unit at all still yields serve's own default, so --health-url is a
	// convenience rather than the only way to recover.
	if got := healthURLFromUnit(filepath.Join(t.TempDir(), "absent")); got != "http://"+service.DefaultListen+"/api/health" {
		t.Errorf("health URL = %q, want the default", got)
	}
}

func TestADegradedServiceStillCountsAsAnAnswer(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		status  int
		body    string
		want    string
		wantErr bool
	}{
		"healthy": {status: http.StatusOK, body: `{"status":"ok","dokkup":"0.2.0"}`, want: "0.2.0"},
		// Dokku being unreachable is not the updater's business, and is no
		// reason to undo a dokkup that is plainly serving.
		"dokku unreachable": {
			status: http.StatusServiceUnavailable,
			body:   `{"status":"degraded","dokku":"unreachable","dokkup":"0.2.0"}`,
			want:   "0.2.0",
		},
		"an answer that names no version": {
			status: http.StatusOK, body: `{"status":"ok"}`, wantErr: true,
		},
		"not dokkup at all": {status: http.StatusOK, body: `<html>`, wantErr: true},
		"an error page":     {status: http.StatusBadGateway, body: `nginx`, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			got, err := probeHealth(t.Context(), srv.Client(), srv.URL)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("probeHealth: %v", err)
			}
			if got != tc.want {
				t.Errorf("version = %q, want %q", got, tc.want)
			}
		})
	}
}

// A slow Dokku must not look like a dokkup that never came up.
//
// The health endpoint spends up to server.healthDokkuTimeout asking Dokku before
// it replies, and the reply is what carries the version this updater is waiting
// for. If the probe gave up first, a wedged Docker would present as "the new
// binary is not answering", and a perfectly good update would be rolled back on
// a host whose only problem is somebody else's. The two budgets are set against
// each other in prose, and nothing else in the suite would notice them drifting.
func TestTheHealthProbeOutwaitsTheServersOwnDokkuTimeout(t *testing.T) {
	t.Parallel()

	if healthProbeTimeout <= server.HealthDokkuTimeout {
		t.Fatalf("the probe gives up after %s but the endpoint may take %s to answer; "+
			"a slow Dokku would be read as a failed update and roll back a working binary",
			healthProbeTimeout, server.HealthDokkuTimeout)
	}

	// Not merely greater: greater with room for the request itself, so that the
	// margin survives an ordinarily slow host.
	if margin := healthProbeTimeout - server.HealthDokkuTimeout; margin < 5*time.Second {
		t.Errorf("only %s separates the two timeouts, which is too little to absorb a slow host", margin)
	}
}

// A health endpoint that takes as long as the server is allowed to take is still
// answered, rather than being read as silence.
func TestAnEndpointThatTakesItsFullBudgetIsStillHeard(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(server.HealthDokkuTimeout):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"degraded","dokku":"unreachable","dokkup":"0.2.0"}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: healthProbeTimeout}
	got, err := probeHealth(t.Context(), client, srv.URL)
	if err != nil {
		t.Fatalf("a reply that took the endpoint's whole budget was missed: %v", err)
	}
	if got != "0.2.0" {
		t.Errorf("version = %q, want %q", got, "0.2.0")
	}
}
