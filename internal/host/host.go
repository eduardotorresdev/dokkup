// Package host is the seam through which dokkup drives the programs that own a
// Linux host's own identities and its proxy.
//
// It exists for the reason internal/dokku and internal/service exist: shelling
// out is fine, but only from one place, so that every process dokkup spawns can
// be found by reading one file, and so that installation is exercised by tests
// that need neither root nor a Dokku host.
//
// The line it draws is "everything installation cannot do as an ordinary user".
// Plain file writes are not here; chown, sudoers and every subprocess are.
//
// See docs/adr/0013-installation-drives-the-hosts-own-tools-through-one-seam.md.
package host

import (
	"context"
	"errors"
	"io/fs"
)

// Absolute paths, because /usr/sbin is absent from the non-root PATH on Ubuntu
// (ENV_PATH in /etc/login.defs) and because sudoers matches on the resolved
// command path, not on what a PATH search would have found.
const (
	Groupadd = "/usr/sbin/groupadd"
	Useradd  = "/usr/sbin/useradd"
	Usermod  = "/usr/sbin/usermod"
	Userdel  = "/usr/sbin/userdel"
	Groupdel = "/usr/sbin/groupdel"
	Visudo   = "/usr/sbin/visudo"
	Runuser  = "/usr/sbin/runuser"
	Sudo     = "/usr/bin/sudo"
	Nginx    = "/usr/sbin/nginx"
)

// Nologin is the shell a service account gets. useradd with this shell and
// --home-dir /nonexistent --no-create-home leaves the password field `!`, which
// `passwd -S` reports as locked, so no separate hardening step is needed.
const Nologin = "/usr/sbin/nologin"

// ErrNoSuchUser and ErrNoSuchGroup report an identity this host does not have.
// They are how a caller tells "Dokku is not installed" from "the lookup
// failed".
var (
	ErrNoSuchUser  = errors.New("host: no such user")
	ErrNoSuchGroup = errors.New("host: no such group")
)

// ErrUserBusy reports that a user could not be removed because a process is
// still running as them. It is userdel's exit 8, and it is actionable: stop the
// service first. Removal must never answer it with --force, which succeeds
// while leaving orphaned processes under a uid the host is free to hand out
// again.
var ErrUserBusy = errors.New("host: a process is still running as that user")

// ErrSudoRefused reports that the sudoers rule installation just wrote does not
// actually let the service user run Dokku.
var ErrSudoRefused = errors.New("host: the sudoers rule does not work")

// Account is a resolved system identity. Uid and Gid are numeric because that
// is what chown takes.
type Account struct {
	Name string
	Uid  int
	Gid  int
}

// UserSpec describes the system user installation creates.
type UserSpec struct {
	// Name is the login name, and also the name of the primary group.
	Name string

	// Group is the primary group, which must already exist. Naming it
	// explicitly rather than letting USERGROUPS_ENAB invent one is what makes
	// userdel later take the group with it -- measured: userdel removes the
	// primary group iff it shares the user's name and has no other members.
	Group string

	// Shell and Home are reconciled on every run, so an account a previous
	// installation left wrong is corrected rather than worked around.
	Shell string
	Home  string
}

// Ops is everything installation and removal need from the host that an
// ordinary user cannot do.
//
// Add a method when a feature needs it, and add it to [Fake] in the same change.
type Ops interface {
	// LookupUser resolves a system user. It returns [ErrNoSuchUser] when the
	// host does not have one, which is not a failure: it is how installation
	// decides whether to create it.
	//
	// The answer comes from os/user, which with CGO_ENABLED=0 compiles to the
	// pure-Go reader and honours `files` alone. /etc/nsswitch.conf on a stock
	// Dokku host is `passwd: files`, measured, so there this is exact; on a
	// host running SSSD or LDAP it could report absent a user `getent`
	// resolves, and creating a local account then shadows a directory one.
	// There is no lookup an installer can substitute that would be better, so
	// the mitigation is the one an installer can actually offer: it reports
	// what it is about to create before it creates it.
	LookupUser(ctx context.Context, name string) (Account, error)

	// LookupGroup resolves a system group, returning [ErrNoSuchGroup] likewise.
	LookupGroup(ctx context.Context, name string) (Account, error)

	// EnsureGroup creates a system group, or does nothing if it exists.
	EnsureGroup(ctx context.Context, name string) error

	// EnsureUser creates the service account, or reconciles an existing one to
	// spec. It reports whether it created the account, so that an installation
	// which fails later undoes only what this run actually did.
	EnsureUser(ctx context.Context, spec UserSpec) (created bool, err error)

	// JoinGroup adds user to a supplementary group, additively. It is safe to
	// repeat. A missing user or group is reported rather than swallowed: it
	// means Dokku is not installed on this host.
	JoinGroup(ctx context.Context, user, group string) error

	// EnsureDir creates a directory and puts it under owner at mode, on every
	// run. Repeating it is how a directory an earlier installation got wrong is
	// corrected.
	EnsureDir(ctx context.Context, path string, mode fs.FileMode, owner Account) error

	// WriteSudoers installs one validated sudoers file. It stages, fixes mode
	// and ownership, validates, and only then puts the file where sudo will
	// read it. A file that does not parse never arrives.
	WriteSudoers(ctx context.Context, path, rule string) error

	// ProveSudo runs program as user, through sudo, as runAs, and returns nil
	// only if it worked. It is how installation proves the sudoers rule rather
	// than assuming it, and it wraps [ErrSudoRefused] when it did not.
	ProveSudo(ctx context.Context, user, runAs, program string, args ...string) error

	// RemoveUser removes a system user, reporting whether there was one to
	// remove. It returns [ErrUserBusy] when a process is still running as them.
	RemoveUser(ctx context.Context, name string) (removed bool, err error)

	// RemoveGroup removes a system group, reporting whether there was one.
	// Removing the user usually takes the group with it, so "there was none"
	// is the ordinary answer here rather than a surprise.
	RemoveGroup(ctx context.Context, name string) (removed bool, err error)

	// CheckProxy runs the proxy's own configuration test over the whole live
	// configuration. Installation calls it before it writes anything, so a
	// configuration this host already had broken is reported as such rather
	// than blamed on dokkup, and again after, so a file dokkup wrote is never
	// left on disk failing it.
	CheckProxy(ctx context.Context) error

	// CheckProxyFragment tests one candidate vhost in isolation, without the
	// live configuration being involved at all.
	CheckProxyFragment(ctx context.Context, path string) error
}
