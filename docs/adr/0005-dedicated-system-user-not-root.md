# The service runs as a dedicated system user, not as root

Installation creates a `dokkup` system user belonging to the `dokku` group, with
a sudoers rule permitting exactly one program without a password: the `dokku`
binary. The systemd unit runs as that user with `ProtectSystem=full`,
`PrivateTmp` and a narrow set of writable paths.

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

## Amended: the sandbox cannot be tighter than what Dokku needs

This decision originally listed `NoNewPrivileges`, `ProtectHome` and
`ProtectSystem=strict`. Writing the unit out in `internal/service` and running it
against a real Dokku showed that all three are incompatible with the sudoers rule
in the same paragraph, for one reason: dokkup does its work by executing `dokku`,
and every sandbox directive here is inherited by that process. Measured against
Dokku 0.38.7 in the development environment:

| Directive | What it does to Dokku |
|---|---|
| `NoNewPrivileges=yes` | `sudo` refuses to run — `The "no new privileges" flag is set` — so dokkup never reaches Dokku at all |
| `ProtectHome=yes` | hides `/home/dokku`, which is `DOKKU_ROOT`, so `apps:list` reports no applications on a host that has them |
| `ProtectSystem=strict` | makes `/var/lib/dokku` read-only, so reads work and `apps:create` fails with `read-only file system` |

Each failure is silent. `dokku version` is the only command `/api/health` runs,
and it keeps succeeding under all three, so the service reports itself healthy
while the interface shows an empty host.

`ProtectSystem=full` is what remains, and it is the honest line to draw: it
protects what dokkup can name — `/usr`, `/boot`, `/etc`, which includes dokkup's
own binary — and leaves `/var` to Dokku, whose writable paths are not dokkup's to
enumerate and would drift as Dokku changes. The hardening was always described
here as defence in depth rather than a security boundary, and this ADR also makes
reaching Dokku the reason the service exists; where the two collide, the
hardening yields. ADR-0012 records the measurements.
