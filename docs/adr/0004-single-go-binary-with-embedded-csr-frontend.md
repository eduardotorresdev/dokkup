# A single Go binary with an embedded client-rendered frontend

The backend is Go and the frontend is SvelteKit built as a static client-rendered
application, compiled into the binary with `embed.FS`. The same binary is both the
server and the CLI. Installing means placing one file; removing it means deleting
one file and one directory.

A runtime dependency on the host — Node, or an interpreter, or a container image —
would contradict the goal of being trivially installable and removable on a
machine the operator already cares about. Go also cross-compiles to arm64 and
amd64 without ceremony, and a pure-Go SQLite driver avoids cgo entirely.

## Consequences

There is no server-side rendering and therefore no server-side route guard. Route
guards in the client are a user-experience affordance only; all authorisation is
enforced by the API, which answers 401 and 403 and lets the client react.

Because there are no SvelteKit form actions to piggyback on, cross-site request
forgery is prevented by requiring a custom request header on every mutation in
addition to a `SameSite` session cookie.

The frontend must be built before the Go binary; the build is not valid otherwise.
