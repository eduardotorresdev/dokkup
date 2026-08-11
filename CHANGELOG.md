# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Released sections below are written by the release job from the
[Conventional Commits](https://www.conventionalcommits.org/) in each cycle, and
are the same text that appears on the release page. Editing one after the fact
makes the two disagree. To say something the commit subjects cannot, write it
under `## [Unreleased]` instead: whatever is there is published as-is, in place
of the generated entries, and the section is emptied once it ships.

## [Unreleased]

## [0.1.0] - 2026-08-11

This is the first release, and it is a foundation rather than a product.
`dokkup serve` and `dokkup update` do their jobs; `dokkup install` prints exactly
what installing would do and then exits saying it is not built yet, so reaching a
Dokku host still means placing the binary and the systemd unit by hand. The web
interface reports whether Dokku is reachable and nothing more.

### Added

- Domain model (`CONTEXT.md`) and the architectural decisions behind it
  (`docs/adr/`)
- Project documentation: README, contributing guide, security policy with an
  explicit threat model, code of conduct
- Development environment running a real Dokku locally, under Apple's container
  runtime on macOS or Docker elsewhere
- Project skeleton: the Go binary with its `install`, `uninstall`, `publish` and
  `serve` subcommands, the Dokku seam and its in-memory fake, and the
  client-rendered frontend embedded into the binary
- Continuous integration, and a release pipeline producing signed binaries
- Releases cut automatically from the Conventional Commits on every merge to
  main, with the version, the changelog section and the release notes all
  derived in the same pass, so the file and the release page cannot disagree;
  `make release-preview` shows what the next merge would publish
- `dokkup update`, which replaces the running binary with a newer one after
  verifying its checksum and its cosign signature, restarts the service, and
  puts the previous binary back if the new one does not answer as healthy on
  the version just installed; `--check` reports whether an update exists and
  changes nothing, so it is safe to run from cron

[unreleased]: https://github.com/eduardotorresdev/dokkup/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/eduardotorresdev/dokkup/releases/tag/v0.1.0
