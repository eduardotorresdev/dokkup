# DokkuClient is the only seam, and it grows on demand

Every call into Dokku goes through the `DokkuClient` interface. It is not a
complete binding of the Dokku CLI and is not intended to become one: a method is
added when a feature needs it, and no sooner.

Mirroring the whole CLI would be a large, permanently out-of-date effort against
a surface dokkup mostly does not use. Growing the interface feature by feature
means that when a milestone is finished, everything dokkup consumes from Dokku is
by construction behind the seam, and the in-memory fake used by tests covers
exactly that same surface without drifting from it.

## Consequences

Commands are built as argument vectors and executed without a shell, so an App
name can never be interpreted as shell syntax. Names are validated against
Dokku's own rules before use.

Machine-readable output (`--format json`) is preferred wherever Dokku offers it;
text parsing is confined to the commands that do not.

Tests run against the fake and need no container, which keeps CI fast. Only the
installer and integration tests need the real dev environment.
