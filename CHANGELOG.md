# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Entries are generated from [Conventional Commits](https://www.conventionalcommits.org/)
at release time.

## [Unreleased]

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
