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

## [0.4.0] - 2026-08-12

### Added

- **store:** hold dokkup's own state in SQLite
- **auth:** the Setup Token, sessions, CSRF and the IP Mode restriction

## [0.3.0] - 2026-08-11

`dokkup install --domain` now gets its own certificate. Pass a name and an
email, and dokkup obtains a certificate for it from Let's Encrypt and renews it
thirty days before expiry, without a plugin and without a certificate file of
yours. It speaks ACME directly: Dokku's Let's Encrypt plugin issues certificates
for apps, dokkup is not an app, and installation still leaves that plugin exactly
as it found it.

Because a certificate authority proves the name by fetching a token from this
host on port 80, and nginx will not start with a certificate file missing,
installation writes a self-signed certificate first, brings the service up, and
waits while the service replaces it — up to ninety seconds, then it says what is
being served in the meantime and where the reason is written down. A certificate
that has not arrived is not a failed installation: the service keeps trying every
fifteen minutes, and nothing has to be run again by hand.

`--cert` and `--key` still work and are never renewed over, because a domain is
not consent to replace a certificate you chose. Passing only one of the two is
refused rather than half-used. Installing without `--acme-email` says so, because
that address is the only warning that reaches you when renewal has been quietly
failing for a month.

Two things this found on the way, both of which made a first installation
fail on a host where nothing was wrong: nginx is now reloaded before the service
starts, so the certificate authority has somewhere to fetch the token from on the
first attempt rather than the second; and a certificate issued after the
authority's answer to the finalize request is now collected from the order,
rather than abandoned on a URL the authority is not obliged to send.

Re-running `dokkup install` on a host that already has it restarts the service,
which it did not before — so a second installation is now actually running the
binary and the flags it just installed.

## [0.2.0] - 2026-08-11

`dokkup install` and `dokkup uninstall` now do what they print. Installation
creates the `dokkup` system user in the `dokku` group, writes a sudoers rule
permitting exactly one program run as exactly one account and then proves that
rule works before reporting success, makes a data directory no other
unprivileged account on the host can even list, puts the binary and the systemd
unit in place, writes one nginx server block, and waits until dokkup answers its
own health endpoint as the version just installed — and answers it through nginx
too, because `systemctl reload` returns before the new configuration is serving.
Running it again changes nothing, and repairs a directory or a rule an earlier
run left wrong. A run that fails part way undoes what that run changed and
nothing it merely found, so a failed re-install cannot destroy a working one.

Removal takes the same list back off, asks before deleting the data directory,
and makes you authenticate again first: your own password, or this host's name
when you are already root. A wrong answer removes nothing at all.
It stops the service before removing the account that service runs as, and
leaves your apps, your volumes, this host's Dokku plugins and Dokku itself as
they were.

Publishing at a domain now needs a certificate you supply — `--domain` together
with `--cert` and `--key`. Dokku's Let's Encrypt plugin issues certificates for
apps and dokkup is not an app, so dokkup cannot obtain one for its own name; for
the same reason the installer no longer installs that plugin, and a host that
has one keeps it and everything renewing through it. Reached at an address
instead, dokkup serves HTTPS on port 8443, because 443 belongs to Dokku's own
catch-all and that refuses connections arriving without a name. The self-signed
certificate's fingerprint is printed in the form a browser shows it, so the
warning can be checked rather than clicked through.

The service now invokes Dokku as `sudo -n -u dokku`, which is the only form that
works for an unprivileged account and is exactly what the sudoers rule permits;
`dokkup serve --dokku-run-as` turns that hop off on a host set up differently.
Creating the owner account still needs a single-use token, which this version
does not issue yet — installation says so, where it will later print one.

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

[unreleased]: https://github.com/eduardotorresdev/dokkup/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/eduardotorresdev/dokkup/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/eduardotorresdev/dokkup/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/eduardotorresdev/dokkup/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/eduardotorresdev/dokkup/releases/tag/v0.1.0
