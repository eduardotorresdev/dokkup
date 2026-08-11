// Package service owns dokkup's systemd unit.
//
// Two things live here, and they belong together: the text of the unit, which
// is the one definition of how dokkup runs on a host (ADR-0001), and the seam
// through which dokkup asks systemd to act on it. Anything that installs,
// updates or removes the service goes through this package, so that there is
// never a second answer to "how is dokkup supposed to run".
//
// See docs/adr/0012-systemd-is-the-second-and-last-thing-dokkup-shells-out-to.md.
package service

import "context"

// Name is the systemd unit dokkup installs. [hostpaths.Unit] is where it goes.
const Name = "dokkup.service"

// Manager is the seam through which dokkup asks systemd to act on its own unit.
//
// It exists for the same reason [dokku.Client] does: shelling out is fine, but
// only from one place. Tests get to watch what was asked without needing a
// service manager, and every invocation is built as an argument vector in a
// single file that can be read in one sitting.
type Manager interface {
	// Restart restarts the unit. It returns once systemd has finished the job,
	// which means the process was started -- not that it is serving. Whether it
	// came back up is a question for the health endpoint, not for systemd.
	Restart(ctx context.Context) error

	// ResetFailed clears a failure systemd is still holding on to.
	//
	// systemd gives up on a unit that has failed too often in too short a time
	// and refuses to start it again until the failure is cleared. That is
	// exactly the state a rollback finds the service in, because the version
	// being undone is the one that crash-looped; without this, restoring a
	// working binary would be refused and a recoverable host would be reported
	// as one that needs a person. It is also the state a host is in when the
	// reason for updating is that the running version is broken.
	//
	// A unit that has not failed is unaffected.
	ResetFailed(ctx context.Context) error

	// IsActive reports whether systemd currently considers the unit running.
	IsActive(ctx context.Context) (bool, error)

	// The four below were added for installation and removal, and ADR-0012
	// points two ways about them. Its consequences say the package "stays this
	// small -- Restart, ResetFailed and IsActive", and also that "a method is
	// added only when a feature needs it", while naming "a change that wants to
	// enable, mask or query unit properties" as a sign the decision is being
	// reopened rather than extended.
	//
	// This is the extension. Installation and removal are the feature that
	// needs them: Enable, Disable and DaemonReload are what installing a unit
	// means, and Reload is here because dokkup writes one file into Dokku's
	// nginx and has to ask nginx to read it. The alternative is a second place
	// that runs systemctl, which is the exact thing ADR-0012 exists to prevent.
	// What has not changed is that this package holds only systemctl, and that
	// nothing is added to it without a feature that needs it.

	// DaemonReload makes systemd read the unit files again. Installation calls
	// it after writing the unit and removal after deleting it; systemd is
	// otherwise still holding the old text.
	DaemonReload(ctx context.Context) error

	// Enable makes the unit start at boot and starts it now. Both halves are
	// what installing a service means, and doing only the first would leave an
	// installation reporting success on a host where dokkup answers nothing
	// until the next reboot.
	Enable(ctx context.Context) error

	// Disable stops the unit and stops it starting at boot.
	//
	// Removal calls it first and cannot skip it: userdel refuses with exit 8
	// while a process is still running as the user. Measured on the devenv
	// against a service account with a live unit still running as it:
	// `userdel: user zz-probe-svc is currently used by process 342857`, exit 8.
	// Answering that with --force is not the fix -- it succeeds while leaving
	// orphaned processes under a uid the host is free to hand out again.
	Disable(ctx context.Context) error

	// Reload asks systemd to reload a unit's configuration without restarting
	// it. It takes a unit name rather than acting on dokkup's own unit because
	// the one caller reloads nginx: dokkup writes a single file into Dokku's
	// nginx and has to ask nginx to read it.
	//
	// `systemctl reload nginx` runs the ExecReload of nginx's own unit, which
	// is `nginx -s reload` after a successful `nginx -t`. Running `nginx -s
	// reload` here instead is the form to avoid, because it bypasses both: it
	// signals the master directly, so systemd never learns whether the reload
	// took, and the configuration is never tested first.
	//
	// `dokku nginx:reload` is deliberately not used either. It is measured to
	// be exactly `nginx -t` followed by `systemctl reload nginx` and nothing
	// else -- there is no hidden Dokku bookkeeping in it -- so routing a
	// host-wide action through the app-scoped [dokku.Client] seam would buy
	// nothing and would widen a seam that is meant to be about apps.
	//
	// Returning is not the same as having reloaded. Measured on the devenv, a
	// vhost removed before the reload was still answering 200 the instant
	// `reload` exited 0, and gone two seconds later, so a caller that needs to
	// know it took has to go and look rather than trust the exit status.
	Reload(ctx context.Context, unit string) error
}
