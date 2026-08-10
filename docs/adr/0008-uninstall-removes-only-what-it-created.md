# Removal touches only what dokkup created

`dokkup uninstall` removes the binary, the systemd unit, the sudoers rule, the
system user and group, and the published vhost. It asks before deleting the data
directory. It never removes Apps, Managed Volumes, networks, Dokku plugins or
Docker credentials.

The installer ensures the Let's Encrypt plugin is present, and removal
deliberately leaves it in place: other Apps on the host almost certainly depend on
it for certificate renewal, and uninstalling a management UI must not silently
break unrelated applications. Removal is not a way to reset the server.

## Consequences

The full list of what will and will not be removed is printed every time, as part
of the command rather than behind a separate flag. Removal then requires
re-authentication: a fresh sudo password prompt where one applies, or typing the
host's name exactly when already running as root.

Dokku plugins are treated as an implementation detail throughout. The interface
never mentions them, and removal neither deletes them nor instructs the operator
to.
