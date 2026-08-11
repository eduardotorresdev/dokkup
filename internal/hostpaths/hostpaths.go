// Package hostpaths names every location on a Dokku host that dokkup owns.
//
// It exists so that the installer and the uninstaller cannot disagree. The
// uninstall contract is that dokkup removes what it created and nothing else,
// and that promise is only keepable if there is exactly one list of what it
// created. See docs/adr/0008-uninstall-removes-only-what-it-created.md.
package hostpaths

import "path/filepath"

// Owned locations, created by installation and removed by uninstallation.
const (
	// Binary is where the dokkup executable is installed.
	Binary = "/usr/local/bin/dokkup"

	// PreviousSuffix marks the executable an update replaced, kept beside the
	// new one so that a version which does not come back healthy can be undone
	// without another download.
	PreviousSuffix = ".previous"

	// PreviousBinary is where that copy lives.
	//
	// It is written by `dokkup update`, and by an installation that found an
	// executable already at [Binary] and replaced it -- both are the same act
	// and there is no reason for two slots. A first installation writes no such
	// file, which is why this is not in [Owned]: the install plan must not
	// claim to write something only some runs write. Removal takes it all the
	// same.
	PreviousBinary = Binary + PreviousSuffix

	// Unit is the systemd service.
	Unit = "/etc/systemd/system/dokkup.service"

	// Sudoers grants the service user permission to run exactly one program.
	Sudoers = "/etc/sudoers.d/dokkup"

	// Vhost is the one file dokkup writes under /etc/nginx. Deleting it and
	// reloading removes dokkup from the proxy entirely.
	//
	// It is not in [Owned] for the same reason [PreviousBinary] is not: only
	// some installations have one. Plain-HTTP IP mode writes it; so does
	// publishing; an installation that declined both does not.
	Vhost = "/etc/nginx/conf.d/dokkup.conf"

	// VhostStaging is where a candidate vhost is validated before it is renamed
	// into place. The leading dot and the absent .conf suffix are both
	// deliberate: nginx.conf includes conf.d/*.conf, so this name cannot be
	// globbed into the live configuration during the window.
	VhostStaging = "/etc/nginx/conf.d/.dokkup.conf.tmp"

	// DataDir holds the database, stored deploy output and any self-signed
	// certificate. Removal asks before deleting it.
	DataDir = "/var/lib/dokkup"

	// TLSDir holds the certificate dokkup serves, whether self-signed or
	// supplied. The paths never change, so a future renewal replaces file
	// contents rather than rewriting the vhost.
	//
	// It sits inside [DataDir] but is removed separately and unconditionally:
	// a certificate is not operator data, and an uninstall that keeps the
	// database has no reason to keep a key.
	TLSDir  = DataDir + "/tls"
	TLSCert = TLSDir + "/cert.pem"
	TLSKey  = TLSDir + "/key.pem"

	// User and Group are the unprivileged system identities the service runs
	// as. See docs/adr/0005-dedicated-system-user-not-root.md.
	User  = "dokkup"
	Group = "dokkup"

	// DokkuGroup is the existing group that grants permission to run dokku.
	// dokkup joins it and never creates or removes it.
	DokkuGroup = "dokku"
)

// Foreign locations dokkup reads or affects but never removes.
const (
	// DokkuStorageRoot is where Dokku keeps application storage. Managed
	// volumes live under it; nothing outside it is ever mounted by dokkup.
	// See docs/adr/0009-managed-volumes-only.md.
	DokkuStorageRoot = "/var/lib/dokku/data/storage"

	// DokkuAppJSONRoot is where Dokku extracts each app's app.json at deploy
	// time. dokkup reads healthcheck definitions from here and never writes.
	DokkuAppJSONRoot = "/var/lib/dokku/data/app-json"
)

// Owned returns every filesystem path installation creates, in the order
// removal should visit them.
func Owned() []string {
	return []string{Unit, Sudoers, Binary, DataDir}
}

// Conditional returns the paths dokkup owns that only some installations have.
//
// They are separate from [Owned] so that the install plan, which prints Owned,
// cannot promise a file this installation will not write -- and they are here
// rather than spelled out at the removal site so that there is still exactly
// one place that knows what dokkup put on a host.
func Conditional() []string {
	return []string{Vhost, TLSDir, PreviousBinary}
}

// Rooted returns path underneath root, for tests that install into a temporary
// directory rather than into /. An empty root returns path unchanged, which is
// what every real installation passes.
func Rooted(root, path string) string {
	if root == "" {
		return path
	}
	return filepath.Join(root, path)
}
