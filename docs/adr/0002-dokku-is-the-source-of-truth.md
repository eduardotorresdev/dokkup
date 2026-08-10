# Dokku is the source of truth; SQLite holds only dokkup's own state

dokkup stores operators, sessions, audit entries, deploy records and settings in
a SQLite database under its data directory, and stores nothing at all about
applications. Everything about Apps is read from Dokku on demand, behind a short
in-memory cache that is invalidated immediately after any mutation dokkup makes.

Apps change through `git push`, through the CLI, and through other operators.
Any persisted mirror of that state would be wrong some of the time, and
reconciling a mirror is an open-ended source of bugs. The cost is that every page
load costs one or more `dokku` invocations, each of which spawns a process.

## Consequences

Read latency is bounded by the Dokku CLI, not by dokkup. Views that need many
facts about many Apps must batch their queries deliberately rather than assuming
reads are free.

The database is a single file, so backup and removal are both trivial — which is
what makes the uninstall contract in ADR-0008 possible to honour.
