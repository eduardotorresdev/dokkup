package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/release"
	"github.com/eduardotorresdev/dokkup/internal/service"
)

// ErrRolledBack reports that a new version did not come back healthy and the
// previous one was put back. The host is serving; it is simply not serving what
// was asked for.
var ErrRolledBack = errors.New("the update was rolled back")

// ErrRollbackFailed reports that a new version did not come back healthy and
// neither did the one restored in its place. This needs a person.
var ErrRollbackFailed = errors.New("the rollback did not restore a working service")

const (
	// defaultHealthTimeout is how long a new binary has to answer. A restart is
	// quick; a minute is generous enough that a slow host is not called broken.
	defaultHealthTimeout = 60 * time.Second

	// healthPollInterval is how often it is asked. Frequently enough that an
	// update feels immediate, rarely enough to be no load at all.
	healthPollInterval = time.Second

	// checkTimeout bounds --check. It runs unattended from cron, where hanging
	// forever is worse than failing.
	checkTimeout = 30 * time.Second

	// healthProbeTimeout bounds one health request.
	//
	// It must stay comfortably above the server's own healthDokkuTimeout, which
	// is how long the endpoint may spend asking Dokku before replying. A wedged
	// Docker makes every reply slow, and if this fired first the updater would
	// see no answer at all, conclude the new binary never came up, and roll back
	// one that was working -- turning somebody else's Dokku problem into a failed
	// dokkup update. Generous here costs a slow poll; mean here costs a rollback.
	healthProbeTimeout = 15 * time.Second
)

// releaseSource is the part of [release.Source] an update uses, named here so
// that tests can supply their own without a network.
type releaseSource interface {
	Latest(ctx context.Context) (release.Release, error)
	Resolve(ctx context.Context, tag string) (release.Release, error)
	Fetch(ctx context.Context, rel release.Release, goarch, dir string) (release.Artifact, error)
}

func runUpdate(env Env, args []string) error {
	fs := newFlagSet(env, "update")
	check := fs.Bool("check", false, "report whether a newer version exists and change nothing")
	version := fs.String("version", "", "install this version instead of the newest")
	allowDowngrade := fs.Bool("allow-downgrade", false, "permit installing an older version than the one running")
	timeout := fs.Duration("timeout", defaultHealthTimeout, "how long to wait for the new version to answer as healthy")
	healthURL := fs.String("health-url", "", "health endpoint to poll (default: the address in the installed unit)")

	if err := parseFlags(fs, args); err != nil {
		return err
	}

	source := &release.Source{}
	up := &updater{
		env:      env,
		source:   source,
		manager:  service.NewSystemctl(),
		binary:   hostpaths.Binary,
		goarch:   runtime.GOARCH,
		running:  env.Build.Version,
		timeout:  *timeout,
		interval: healthPollInterval,
	}

	if *check {
		ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
		defer cancel()
		return up.check(ctx, *version)
	}

	if err := preflight(); err != nil {
		return err
	}

	url := *healthURL
	if url == "" {
		url = healthURLFromUnit(hostpaths.Unit)
	}
	client := &http.Client{Timeout: healthProbeTimeout}
	up.health = func(ctx context.Context) (string, error) { return probeHealth(ctx, client, url) }

	return up.update(context.Background(), *version, *allowDowngrade)
}

// preflight refuses an update this host cannot carry out, before anything has
// been downloaded and while saying so still costs nothing.
func preflight() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("dokkup runs on Linux hosts; this is %s", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		return errors.New("run as root: this replaces a system binary and restarts a system service")
	}

	info, err := os.Stat(hostpaths.Binary)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not there, so dokkup does not look installed on this host; "+
			"install it first with the instructions in the README", hostpaths.Binary)
	}
	if _, err := os.Stat(hostpaths.Unit); err != nil {
		return fmt.Errorf("%s is not there, so there is no service to restart; "+
			"dokkup does not look installed on this host", hostpaths.Unit)
	}
	return nil
}

// updater performs one update. Everything it touches the outside world through
// is a field, so the whole of it -- including the rollback -- is exercised by
// tests that need neither root, nor systemd, nor a network.
type updater struct {
	env     Env
	source  releaseSource
	manager service.Manager

	// health reports which dokkup answered the health endpoint.
	health func(ctx context.Context) (string, error)

	// binary is the installed executable to replace.
	binary string

	// goarch is the architecture to download for.
	goarch string

	// running is the version of the binary asking for the update.
	running string

	timeout  time.Duration
	interval time.Duration
}

// check reports what an update would do, and does nothing.
//
// It exits zero whether or not an update exists: an available update is news,
// not a failure. Only a check that could not be made at all is an error, which
// is what lets a cron tell "up to date" from "could not reach GitHub".
func (u *updater) check(ctx context.Context, want string) error {
	target, err := u.target(ctx, want)
	if err != nil {
		return err
	}

	switch {
	case !release.IsRelease(u.running):
		printf(u.env.Stdout, "This is a development build (%s). The newest release is %s.\n",
			u.running, target.Version())
	case release.SameVersion(target.Version(), u.running):
		printf(u.env.Stdout, "dokkup %s is running, which is the newest release.\n", u.running)
	case release.Newer(target.Version(), u.running):
		printf(u.env.Stdout, "dokkup %s is running. %s is available.\n", u.running, target.Version())
		printf(u.env.Stdout, "Install it with 'sudo dokkup update'.\n")
	default:
		printf(u.env.Stdout, "dokkup %s is running, which is newer than the %s on the releases page.\n",
			u.running, target.Version())
	}
	return nil
}

func (u *updater) target(ctx context.Context, want string) (release.Release, error) {
	if want != "" {
		return u.source.Resolve(ctx, want)
	}
	return u.source.Latest(ctx)
}

// update replaces the running binary and proves the replacement works.
func (u *updater) update(ctx context.Context, want string, allowDowngrade bool) error {
	target, err := u.target(ctx, want)
	if err != nil {
		return err
	}

	switch {
	case release.SameVersion(target.Version(), u.running) && release.IsRelease(u.running):
		printf(u.env.Stdout, "dokkup %s is already installed. Nothing to do.\n", u.running)
		return nil

	case !release.IsRelease(u.running):
		// A build from a working tree has no place on the version line, so
		// whether it is "older" is unanswerable. Say so and carry on: the
		// operator asked for a released version and is about to get one.
		printf(u.env.Stdout, "Replacing a development build (%s) with the released %s.\n",
			u.running, target.Version())

	case !release.Newer(target.Version(), u.running) && !allowDowngrade:
		return fmt.Errorf("refusing to replace %s with the older %s; pass --allow-downgrade to do it anyway",
			u.running, target.Version())
	}

	u.printPlan(target)

	dir := filepath.Dir(u.binary)
	release.SweepTemp(dir)

	printf(u.env.Stdout, "Downloading %s...\n", target.Tag)
	artifact, err := u.source.Fetch(ctx, target, u.goarch, dir)
	if err != nil {
		return err
	}
	// From here the artifact is either installed or removed. Nothing verified
	// is left lying next to the binary for a later run to find.
	installed := false
	defer func() {
		if !installed {
			_ = os.Remove(artifact.Path)
		}
	}()

	if artifact.Signed {
		printf(u.env.Stdout, "Checksum and signature verified.\n")
	} else {
		printf(u.env.Stdout, "Checksum verified. cosign is not installed, so the signature was not\n")
		printf(u.env.Stdout, "checked: the bytes are the published ones, but nothing here has vouched\n")
		printf(u.env.Stdout, "for who published them.\n")
	}

	previous, err := u.swap(artifact.Path)
	if err != nil {
		return err
	}
	installed = true
	printf(u.env.Stdout, "Installed %s, keeping the previous version at %s.\n", target.Version(), previous)

	if err := u.restart(ctx); err != nil {
		return u.rollback(ctx, previous, fmt.Errorf("restarting the service: %w", err))
	}

	printf(u.env.Stdout, "Waiting for dokkup %s to answer...\n", target.Version())
	if err := u.waitHealthy(ctx, target.Version()); err != nil {
		return u.rollback(ctx, previous, err)
	}

	printf(u.env.Stdout, "\ndokkup %s is running.\n", target.Version())
	return nil
}

// restart clears any failure systemd is still holding on to, then starts the
// service.
//
// The clearing is best effort. A unit that has not failed is unaffected by it,
// and if it does fail then attempting the restart anyway produces the error
// actually worth reporting.
func (u *updater) restart(ctx context.Context) error {
	_ = u.manager.ResetFailed(ctx)
	return u.manager.Restart(ctx)
}

// swap puts the downloaded binary in place, keeping the one it replaced.
//
// The backup is a hardlink, so it costs no copy and cannot half-succeed, and it
// is made before the rename rather than after: a crash between the two leaves a
// backup identical to the current binary, which is harmless, where the other
// order would leave the installation with no way back.
//
// The rename is atomic and is safe over a running executable. Linux keeps the
// old inode alive for the process already running from it, and ETXTBSY is about
// writing to a busy binary, not about replacing its directory entry.
func (u *updater) swap(downloaded string) (string, error) {
	previous := u.binary + hostpaths.PreviousSuffix

	if err := os.Remove(previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("clearing the previous backup at %s: %w", previous, err)
	}
	if err := os.Link(u.binary, previous); err != nil {
		return "", fmt.Errorf("keeping a copy of the running binary: %w", err)
	}
	if err := os.Rename(downloaded, u.binary); err != nil {
		// The installation is untouched: the rename is what would have changed
		// it, and it did not happen.
		_ = os.Remove(previous)
		return "", fmt.Errorf("installing to %s: %w", u.binary, err)
	}
	return previous, nil
}

// waitHealthy polls until the service reports the version just installed.
//
// Answering at all is not enough. A restart that silently kept the old binary
// answers exactly as happily as one that took the new one, so what is waited
// for is the endpoint naming the new version. A degraded reply still counts:
// that means Dokku is unreachable, which is not the updater's business and not
// a reason to undo a working binary.
func (u *updater) waitHealthy(ctx context.Context, want string) error {
	ctx, cancel := context.WithTimeout(ctx, u.timeout)
	defer cancel()

	ticker := time.NewTicker(u.interval)
	defer ticker.Stop()

	var last error
	for {
		got, err := u.health(ctx)
		switch {
		case err != nil:
			last = err
		case release.SameVersion(got, want):
			return nil
		default:
			last = fmt.Errorf("the service is answering as %s, not %s", got, want)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("dokkup %s did not answer as healthy within %s: %w", want, u.timeout, last)
		case <-ticker.C:
		}
	}
}

// rollback puts the previous binary back and restarts, then reports which of
// the two outcomes happened. The distinction matters: "you are fine, on the old
// version" and "this host may have no working binary" deserve different pages
// in the middle of the night.
func (u *updater) rollback(ctx context.Context, previous string, cause error) error {
	printf(u.env.Stderr, "\nThe new version did not come up: %v\n", cause)
	printf(u.env.Stderr, "Putting the previous binary back.\n")

	if err := os.Rename(previous, u.binary); err != nil {
		return fmt.Errorf("%w: restoring %s from %s: %w; the service is stopped and this host needs attention",
			ErrRollbackFailed, u.binary, previous, err)
	}

	if err := u.restart(ctx); err != nil {
		return fmt.Errorf("%w: the previous binary is back at %s but the service would not restart: %w",
			ErrRollbackFailed, u.binary, err)
	}

	if err := u.waitHealthy(ctx, u.running); err != nil {
		return fmt.Errorf("%w: the previous binary is back at %s and is not answering either: %w",
			ErrRollbackFailed, u.binary, err)
	}

	return fmt.Errorf("%w: dokkup %s is running again, and the update did not take", ErrRolledBack, u.running)
}

// printPlan says what is about to happen, and what is not. An operator has no
// other way to know that replacing a binary leaves their state alone.
func (u *updater) printPlan(target release.Release) {
	printf(u.env.Stdout, "dokkup update will:\n\n")
	printf(u.env.Stdout, "  Download %s for linux/%s, check it against SHA256SUMS, and check its\n",
		target.Tag, u.goarch)
	printf(u.env.Stdout, "  signature if cosign is installed. Nothing is installed unless both pass.\n\n")
	printf(u.env.Stdout, "  Replace %s, keeping the version it replaces beside it.\n\n", u.binary)
	printf(u.env.Stdout, "  Restart %s and wait for it to answer as %s. If it does not,\n",
		service.Name, target.Version())
	printf(u.env.Stdout, "  the previous version goes back and the service is restarted again.\n\n")

	printf(u.env.Stdout, "  It will NOT touch:\n\n")
	printf(u.env.Stdout, "    %s -- operators, audit trail, deploy history\n", hostpaths.DataDir)
	printf(u.env.Stdout, "    %s -- how the service runs is unchanged\n", hostpaths.Unit)
	printf(u.env.Stdout, "    %s\n", hostpaths.Sudoers)
	printf(u.env.Stdout, "    your apps, and nothing they run\n\n")
}

// healthURLFromUnit works out where to look for health.
//
// The address is not written down anywhere else, so the unit is the authority:
// it is what decides how the service runs. An operator who edited it gets the
// endpoint they actually bound, and one who removed the flag gets serve's own
// default, which is what serve would then have used.
func healthURLFromUnit(unitPath string) string {
	listen := service.DefaultListen
	// The only caller passes [hostpaths.Unit], a constant; the parameter exists
	// so that the unit can be read from a temporary file in tests.
	if content, err := os.ReadFile(unitPath); err == nil { //nolint:gosec // not a caller-supplied path
		if found, ok := service.ListenFromUnit(string(content)); ok {
			listen = found
		}
	}

	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen + "/api/health"
	}
	// A wildcard is an address to listen on, not one to connect to.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	// Plain HTTP on purpose: Dokku's nginx terminates TLS in front of dokkup
	// (ADR-0006), so this is the same loopback address serve bound.
	return "http://" + net.JoinHostPort(host, port) + "/api/health"
}

// probeHealth asks the running service which dokkup answered.
//
// A degraded reply is still an answer. The endpoint reports 503 when Dokku is
// unreachable, and refusing to accept that would make an unrelated Dokku
// problem roll back a perfectly good dokkup.
func probeHealth(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}

	var body struct {
		Dokkup string `json:"dokkup"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("reading the health response: %w", err)
	}
	if body.Dokkup == "" {
		return "", fmt.Errorf("%s answered without saying which dokkup it is", url)
	}
	return body.Dokkup, nil
}
