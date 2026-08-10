# Publishing at a domain is the recommended path; IP mode is degraded on purpose

The installer asks for a domain and publishes dokkup through Dokku's proxy with a
Let's Encrypt certificate. It first checks that the domain resolves to the host's
public address, so a certificate is never attempted against a domain that cannot
pass the challenge.

When there is no domain, dokkup can still be reached by IP address. No certificate
authority will vouch for an IP address, so the installer offers a self-signed
certificate — printing its fingerprint so the browser warning can actually be
verified rather than clicked through — and falls back to plain HTTP only if that
is declined.

## Consequences

In IP Mode dokkup refuses to create additional operators. The mode is for a single
person setting up or running a private box, and multi-operator use requires a
transport that a browser can authenticate.

Every screen in IP Mode carries a persistent warning naming the mode and the
command that leaves it. The warning is not dismissible; the way to remove it is to
publish a domain.
