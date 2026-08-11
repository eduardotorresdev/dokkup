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
}
