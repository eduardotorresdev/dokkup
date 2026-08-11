// Package hostpaths names every location on a Dokku host that dokkup owns.
//
// It exists so that the installer and the uninstaller cannot disagree. The
// uninstall contract is that dokkup removes what it created and nothing else,
// and that promise is only keepable if there is exactly one list of what it
// created. See docs/adr/0008-uninstall-removes-only-what-it-created.md.
package hostpaths

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
	// It is created by `dokkup update` rather than by installation, which is
	// why it is not in [Owned]: the install plan must not claim to write a file
	// installation never writes. Removal has to take it all the same.
	PreviousBinary = Binary + PreviousSuffix

	// Unit is the systemd service.
	Unit = "/etc/systemd/system/dokkup.service"

	// Sudoers grants the service user permission to run exactly one program.
	Sudoers = "/etc/sudoers.d/dokkup"

	// DataDir holds the database, stored deploy output and any self-signed
	// certificate. Removal asks before deleting it.
	DataDir = "/var/lib/dokkup"

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
