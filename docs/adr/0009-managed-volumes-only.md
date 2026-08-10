# Only managed volumes; arbitrary host paths are refused

dokkup creates and mounts storage only under Dokku's own storage root, using
Dokku's directory helper with the ownership its builder expects. Mounting an
arbitrary path from the host is not offered.

Dokku itself permits any host path. Exposing that through a web form turns a
storage feature into a general read-write primitive over the whole filesystem,
reachable from a browser session — and leaves data scattered in places removal
cannot account for. Operators who genuinely need an external disk still have the
CLI, which is unaffected.

## Consequences

Destroying an App does not destroy its data: Dokku leaves the storage directory
behind. dokkup shows exactly which Managed Volumes will survive, with their size
on disk, and offers deleting them as a separate opt-in choice rather than folding
an irreversible act into a reversible one.
