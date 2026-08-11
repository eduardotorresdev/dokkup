# dokkup

A web interface for the day-to-day operation of a [Dokku](https://dokku.com)
host — environment variables, domains and certificates, logs, storage,
connections between apps — without opening a terminal for each of them.

dokkup installs as a single binary, keeps no copy of your applications' state,
and removes itself without leaving anything behind.

> **Status: being built.** `dokkup serve` and `dokkup update` work today;
> everything else below — including `dokkup install`, which the Installing
> section describes — is the agreed milestone rather than working software, and
> exits saying so. Releases are cut automatically from `main`, so the download
> and verification steps are real. See [docs/adr](docs/adr) for how it is being
> built and why.

## What it does

| Area | Scope |
|------|-------|
| **Apps** | Create and remove, scale process types, start/stop/restart |
| **Deploy** | `git push` (the remote is shown ready to copy), rebuild, and deploy from a Docker image |
| **Environment** | Edit many variables at once and restart once, values masked by default |
| **Domains** | Add and remove domains, enable HTTPS with automatic renewal |
| **Healthchecks** | Control whether checks gate a deploy and their timing; display what the app actually probes |
| **Storage** | Create and attach persistent volumes with the right ownership for the app's builder |
| **Networking** | Connect two apps and get the hostname they reach each other by |
| **Logs** | Live output per process, plus stored output of deploys dokkup ran itself |
| **Operators** | Sign-in accounts for the people who manage the host, with an audit trail |

## What it deliberately does not do

- **Databases and other Dokku services.** Provisioning Postgres, Redis and the
  rest is out of scope for now.
- **Writing to your applications' repositories.** A healthcheck's path lives in
  `app.json`, inside the app's own repository. dokkup shows it and never changes
  it.
- **Mounting arbitrary host paths.** Storage lives under Dokku's storage root so
  that it can be accounted for. See [ADR-0009](docs/adr/0009-managed-volumes-only.md).
- **Per-app permissions.** Anyone who can run `dokku` is root-equivalent on the
  machine, so scoped roles would be a promise dokkup cannot keep. Operators exist
  for accountability, not containment.
- **Managing more than one host.** One installation, one Dokku host.

## Requirements

- A Dokku host, Dokku 0.38 or newer
- Root or `sudo` access on that host, to install
- A domain pointing at the host, to run with a valid certificate

## Installing

Download the binary for your architecture from the
[releases page](https://github.com/eduardotorresdev/dokkup/releases), verify it,
and run the installer:

```sh
curl -fsSLO https://github.com/eduardotorresdev/dokkup/releases/latest/download/dokkup_linux_amd64
curl -fsSLO https://github.com/eduardotorresdev/dokkup/releases/latest/download/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS

sudo install -m 0755 dokkup_linux_amd64 /usr/local/bin/dokkup
sudo dokkup install
```

Releases are signed with [cosign](https://github.com/sigstore/cosign) using
keyless signing, so you can verify where a binary was built:

```sh
cosign verify-blob dokkup_linux_amd64 \
  --bundle dokkup_linux_amd64.cosign.bundle \
  --certificate-identity-regexp 'https://github\.com/eduardotorresdev/dokkup/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

If you would rather not do that by hand, this does the same thing — download,
checksum, signature, then the installer:

```sh
curl -fsSL https://raw.githubusercontent.com/eduardotorresdev/dokkup/main/install.sh | sudo sh
```

`dokkup install` walks through the setup: it asks for a domain, checks that DNS
actually points at this host before requesting a certificate, and prints a
single-use token. Open dokkup in a browser, redeem the token, and you have the
owner account.

Without a domain, dokkup can be reached at the host's IP address instead. No
certificate authority will vouch for an IP, so the installer offers a self-signed
certificate and prints its fingerprint for you to check. In this mode dokkup
restricts itself to the owner alone and shows a warning on every screen. Run
`dokkup publish <domain>` when DNS is ready to leave it.

## Updating

```sh
sudo dokkup update
```

It resolves the newest release, downloads the binary for this host's
architecture along with `SHA256SUMS`, and verifies the checksum before anything
is written. If cosign is installed the signature is verified too; if it is not,
`update` says so and carries on with the checksum alone, exactly as `install.sh`
does.

Only a verified binary is swapped in. The previous one is kept, the service is
restarted, and dokkup then polls `/api/health` until it answers reporting the
version just installed. Reporting the version is the point of the check: a
service that came back on the old binary answers just as happily as one that
came back on the new one. If the new version does not become healthy within the
timeout (`--timeout`, 60s by default), the previous binary is put back and the
service restarted again; exit code 4 says the update failed and the old version
is serving. Exit code 5 says the restored binary did not come back either, and
that host needs a person.

To ask whether a newer version exists without changing anything:

```sh
dokkup update --check
```

This changes nothing, prompts for nothing and needs no root, so it is safe to
run from cron. It exits 0 whether or not an update is waiting, because an
available update is not a failure, and exits 1 only when the check could not be
made at all. That is what lets a cron entry tell "up to date" apart from "could
not reach GitHub".

To install a particular version rather than the newest:

```sh
sudo dokkup update --version v0.3.0
```

Moving to a version older than the running one is refused unless you also pass
`--allow-downgrade`.

`update` replaces the binary and nothing else: not the data directory, not the
systemd unit, not the sudoers rule. Updating must not quietly redo the
installation.

## Removing

```sh
sudo dokkup uninstall
```

It prints everything it is about to remove, and everything it is leaving alone
and why, before asking you to authenticate again. It removes the binary, the
service, the sudoers rule, the system user and the published vhost, and asks
before deleting its data directory.

It does not remove your apps, your volumes, your networks, Dokku plugins or
Docker credentials. Uninstalling a management interface must not break the
server it was managing.

## Security

Read [SECURITY.md](SECURITY.md) before exposing dokkup to a network. The short
version: **an authenticated operator is root-equivalent on the Dokku host, by
design.** Creating a dokkup operator is handing out root on that machine. Treat
the operator list accordingly.

To report a vulnerability, use
[private vulnerability reporting](https://github.com/eduardotorresdev/dokkup/security/advisories/new).
Please do not open a public issue.

## Contributing

Contributions are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) covers the
development environment — a real Dokku running locally in a container — the
commit conventions, and the weight budget the project holds itself to.

## License

[Apache License 2.0](LICENSE).

dokkup is not affiliated with or endorsed by the Dokku project.
