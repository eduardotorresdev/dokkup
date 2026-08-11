# Contributing to dokkup

Thanks for taking the time. This document covers how to get a real Dokku running
on your machine, how the code is organised, and what a mergeable change looks
like.

## Getting set up

You need:

- **Go 1.26+** and **Bun 1.3+**
- A container runtime. On macOS with Apple Silicon, Apple's
  [`container`](https://github.com/apple/container) works and is what the
  Makefile prefers. Docker works everywhere else. The Makefile picks whichever it
  finds.

```sh
git clone https://github.com/eduardotorresdev/dokkup
cd dokkup
make devenv-up      # builds the image and starts a real Dokku, first run takes a while
make dev            # backend with live reload, frontend on http://localhost:5173
```

`make devenv-up` starts one container running Ubuntu with systemd as PID 1,
Docker underneath it, and Dokku installed from the upstream bootstrap script. It
is a real Dokku, not a simulation. `make devenv-shell` drops you into it, where
`dokku apps:list` behaves exactly as it does on a server.

That this works at all is surprising enough to be written down — see
[ADR-0011](docs/adr/0011-dev-environment-runs-dokku-inside-apple-container.md)
for what was verified and what does not work.

Useful targets:

| Target | What it does |
|--------|--------------|
| `make dev` | Backend with reload, frontend dev server |
| `make build` | Frontend, then the binary with the frontend embedded |
| `make test` | Go tests against the in-memory Dokku fake — no container needed |
| `make test-integration` | Tests against the real Dokku in the dev environment |
| `make lint` | `golangci-lint`, `svelte-check`, `gofmt` |
| `make tools` | Install the pinned `golangci-lint`, which `make lint` skips if absent |
| `make devenv-shell` | A shell inside the dev environment |
| `make devenv-down` | Stop and remove it |

## How the code is laid out

```
cmd/dokkup/        The single binary: install, uninstall, publish, serve
internal/dokku/    The one seam through which Dokku is invoked, plus the fake
internal/server/   HTTP handlers, sessions, CSRF
internal/store/    SQLite: operators, sessions, audit, deploy records
web/               SvelteKit, client-rendered, embedded into the binary
devenv/            The development environment image
```

Three things about this layout are load-bearing:

**Everything that touches Dokku goes through `internal/dokku`.** Nothing else
shells out. The interface covers only what dokkup actually uses and grows when a
feature needs it — we are not building a complete binding of the Dokku CLI. When
you add a method, add it to the fake in the same change, or the tests will drift
away from reality. See
[ADR-0003](docs/adr/0003-dokkuclient-is-the-only-seam.md).

**Commands are built as argument vectors, never as shell strings.** No
`sh -c`, no string interpolation into a command line. App names are validated
before use. This is the difference between a web form and a remote shell.

**Dokku owns the truth about apps; we store none of it.** dokkup's database holds
operators, sessions, the audit trail, deploy records and settings, and nothing
else. If you find yourself wanting to persist an app's state, that is the signal
to re-read [ADR-0002](docs/adr/0002-dokku-is-the-source-of-truth.md) first.

**There is no server-side rendering, so there is no server-side route guard.**
Guards in the frontend are a user-experience affordance. Authorisation is
enforced in the API or it is not enforced.

## The weight budget

"Lightweight" is a measurement, not an aspiration. dokkup holds itself to:

| | Budget |
|---|---|
| Binary size | ≤ 25 MB |
| Resident memory, idle | ≤ 50 MB |
| Cold start to serving | < 1 s |

CI fails a pull request that pushes any of these over budget. If a change needs
more room, say why in the pull request and we will discuss moving the number
rather than quietly ignoring it.

Adding a dependency is a decision, not a detail. Say in the pull request what it
buys and what it costs.

## Commits

**Sign off your commits.** dokkup uses the
[Developer Certificate of Origin](https://developercertificate.org/) rather than
a contributor licence agreement. `git commit -s` adds the line; that is the whole
process.

**Use [Conventional Commits](https://www.conventionalcommits.org/).** The
changelog and the next version number are derived from them, by a script rather
than by a person — see [Releases](#releases) for what each type does.

```
feat(apps): add process type scaling
fix(dokku): quote app names when building argument vectors
docs(security): state the trust boundary explicitly
```

Types in use: `feat`, `fix`, `docs`, `refactor`, `test`, `build`, `ci`, `chore`.
A breaking change gets a `!` after the type and a `BREAKING CHANGE:` footer.

## Pull requests

Before opening one:

```sh
make lint
make test
make build
```

In the description, say what changes for someone using dokkup. If the change
touches installation, removal, authentication or anything that runs a `dokku`
command, say what you did to convince yourself it is safe — `make
test-integration` output is good evidence.

For anything larger than a bug fix, open an issue first. It is a poor trade to
write a feature and then discover it was out of scope.

## Releases

**Merging to `main` publishes a release.** There is no separate step and nobody
cuts one by hand. The Release workflow reads the Conventional Commits since the
last tag, works out the next version, runs the whole test suite, and only then
tags and publishes. A merge that fails the suite is never tagged.

Only two types move the version, and it is worth knowing which:

| Type | Version | Changelog section |
|---|---|---|
| `feat:` | minor — `0.3.1` → `0.4.0` | Added |
| `fix:` | patch — `0.3.1` → `0.3.2` | Fixed |
| `!` or a `BREAKING CHANGE:` footer | major, **clamped to minor while the major is 0** | as its type |
| `docs:` | none | Documentation |
| `refactor:`, `build:`, `perf:`, anything else | none | Changed |
| `chore:`, `test:`, `ci:` | none | omitted entirely |

The consequence to watch for: **a cycle of nothing but `refactor:` or `perf:`
commits publishes no release at all**, and its changelog entries wait until the
next `feat:` or `fix:` carries them out. If work deserves to reach people, it
needs a commit that says `feat:` or `fix:`.

A merge that releases nothing is a success, not a failure. The workflow says so
and stops; do not read the absence of a release as something to fix.

See what your merge would publish before you open the pull request:

```sh
make release-preview
```

It prints the version and diffs the changelog section it would write, and
changes nothing.

**When a commit subject cannot carry the news**, write the entry yourself under
`## [Unreleased]` in `CHANGELOG.md`. Anything there is published verbatim in
place of the generated entries, and the section is emptied once it ships. That is
the escape hatch for a release that needs a paragraph rather than a list.

**If a release half-happens.** The tag and the branch are pushed atomically, so
they move together or not at all. The one state that needs a person is a tag that
pushed while goreleaser then failed — the release page will be missing or empty.
Delete the tag and push it again from your own credentials, which re-runs the
tag-triggered path:

```sh
git push --delete origin vX.Y.Z
git push origin vX.Y.Z
```

Do not retry by re-running the workflow on a newer `main`: the artifacts would be
built from a tree that is no longer the one the tag names.

## Reporting bugs

Include your dokkup version, your Dokku version, your host operating system, what
you expected and what happened. If a `dokku` command was involved, the exact
command and its output are worth more than a description of them.

**Do not report security vulnerabilities as issues.** See [SECURITY.md](SECURITY.md).

## Code of conduct

Participating means agreeing to the [Code of Conduct](CODE_OF_CONDUCT.md).
