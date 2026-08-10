# The service runs as a dedicated system user, not as root

Installation creates a `dokkup` system user belonging to the `dokku` group, with
a sudoers rule permitting exactly one program without a password: the `dokku`
binary. The systemd unit runs as that user with `NoNewPrivileges`,
`ProtectSystem=strict` and a narrow set of writable paths.

This is defence in depth, not a security boundary. Anyone who can run `dokku` can
mount host paths and talk to Docker, which is root-equivalent in practice. The
distinction that matters is narrower and still worth having: a flaw in the HTTP
layer yields the surface of one binary rather than arbitrary execution as root.

## Consequences

The trust boundary is stated plainly in `SECURITY.md`: an authenticated Operator
is root-equivalent on the Dokku Host by design. Reports that an authenticated
operator can reach the host are not vulnerabilities; reports that an
unauthenticated party can are.

Because the privilege is root-equivalent, dokkup does not offer per-App roles.
Multiple operators exist for accountability and individual revocation, not for
containment.
