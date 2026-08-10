# dokkup runs as a systemd service on the Dokku host

dokkup needs to invoke the `dokku` binary, which only exists on the Dokku Host,
so it runs there as a systemd service rather than as a Dokku app, a Dokku plugin,
or a remote client reaching the host over SSH.

Running as a Dokku app would have given us domains, TLS and restarts for free,
but it makes the management UI unavailable in exactly the situation where it is
most wanted — when Dokku itself is unhealthy. A plugin is more invasive than the
job requires and harder to remove cleanly. Reaching the host over SSH from
elsewhere avoids any footprint on the server, but introduces key management, a
round trip per command, and a second machine holding host credentials.

## Consequences

An installation manages exactly one Dokku Host. There is no notion of a server or
instance in the domain model, and adding multi-host support later means revisiting
this decision rather than extending it.

Because Dokku's nginx already owns ports 80 and 443, dokkup cannot terminate TLS
on its own with an embedded ACME client. Publishing at a domain therefore goes
through Dokku's proxy — see ADR-0006.
