# Security Policy

## Reporting a vulnerability

Report privately through
[GitHub private vulnerability reporting](https://github.com/eduardotorresdev/dokkup/security/advisories/new).
Please do not open a public issue for a suspected vulnerability.

Include what you were able to do, the steps to reproduce it, and the dokkup and
Dokku versions involved. A proof of concept helps a great deal.

You can expect an acknowledgement within 5 working days and an assessment within
15. If a report is accepted, we will agree a disclosure date with you and credit
you in the advisory unless you prefer otherwise.

## Supported versions

Only the latest minor release receives security fixes. dokkup is young and moves
quickly; there is no long-term support branch.

## Threat model

Read this before deciding whether something is a vulnerability. dokkup's trust
boundary is unusual, and the unusual part is deliberate.

**An authenticated operator is root-equivalent on the Dokku host, by design.**

dokkup runs on the Dokku host and invokes the `dokku` binary. Anyone who can run
that binary can mount host paths into containers and instruct Docker, and either
of those is a path to full control of the machine. This is the same authority
held by anyone with shell access to run `dokku`, and it is inherent to what
dokkup is for. Creating a dokkup operator is equivalent to granting root on that
machine.

dokkup narrows this where it can — the service runs as a dedicated unprivileged
user whose sudo rule permits exactly one binary, storage is confined to Dokku's
own storage root, and every action is recorded in an audit trail — but none of
that is a boundary an operator cannot cross. It is damage limitation for flaws
in dokkup, not a sandbox around the operator.

### In scope

- Performing any action without authenticating
- Bypassing or forging authentication, or fixing, stealing or extending a session
- Escalating from one operator to the owner, including reissuing a setup token
  while an owner already exists
- Injecting arguments or commands into an invocation of `dokku`
- Cross-site scripting, cross-site request forgery, or clickjacking
- Secrets appearing where they should not: logs, the audit trail, error
  responses, or the interface when masked
- Reading or writing another operator's data, or any data, without authorisation
- Denial of service reachable without authentication
- Flaws in the installer or the uninstaller: unsafe file permissions, a sudoers
  rule broader than intended, a predictable setup token, or removal deleting
  something it does not own
- Tampering with releases, the signature chain, or the install script

### Not in scope

- An authenticated operator using operator powers: creating or removing apps,
  editing environment variables, mounting managed volumes, reading logs. This is
  the product working as intended.
- Reaching the host filesystem through Dokku features that dokkup exposes to
  authenticated operators
- Vulnerabilities in Dokku, Docker, nginx or the host operating system. Report
  those upstream; if dokkup makes one materially easier to exploit, that part is
  in scope here.
- Missing hardening with no demonstrated impact, such as a header or a cookie
  flag on a response that carries nothing sensitive
- Running dokkup in IP mode over plain HTTP. The interface warns about this
  continuously and restricts itself to the owner; choosing it anyway is a
  documented trade-off, not a flaw.
- Attacks requiring an already-compromised host or an already-compromised
  operator credential
- Automated scanner output with no demonstrated impact

## No bug bounty

There is no monetary reward. Accepted reports are credited in the advisory and in
the release notes.
