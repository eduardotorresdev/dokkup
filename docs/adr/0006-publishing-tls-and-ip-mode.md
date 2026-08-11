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

## Amended: the certificate for dokkup's own domain is one you supply

The opening paragraph says the installer publishes dokkup "with a Let's Encrypt
certificate". Building installation for real established that it cannot, and the
reason is structural rather than a missing feature.

Dokku's Let's Encrypt plugin issues certificates for Apps. Measured against
`dokku-letsencrypt` 0.25.1 in the development environment — Ubuntu 24.04, Dokku
0.38.7, linux/arm64 — every entry point begins by calling `verify_app_name` and
exits 20 for an argument that is not an App: `letsencrypt:enable`, `:active`,
`:disable`, `:revoke` and `:cleanup` all take `<app>`, and so do `certs:add`,
`certs:generate` and `certs:remove`. dokkup is not an App and cannot be one —
ADR-0001 puts it beside Dokku as a systemd service rather than inside it — so
there is no argument that plugin will accept on dokkup's behalf.

`dokkup install --domain` therefore requires `--cert` and `--key`, and refuses
without them rather than promising a certificate it has no way to get. The two
files are read, paired and checked to cover the domain before anything is
written, and they are installed at fixed paths under `/var/lib`, so that a
renewal replaces file contents rather than rewriting the server block.

The route back to what this ADR originally promised is an RFC 8555 client of
dokkup's own, and the architecture allows it: HTTP-01 validation by webroot was
measured working through a server block dokkup owns, with nginx serving a token
dokkup had written, because Dokku keeps port 80 and a name dokkup claims there
is routed like any other. That is its own issue, not this one.

## Amended: IP mode with a certificate is served on port 8443

This ADR's second paragraph offers a self-signed certificate without saying
where it is served, and `https://<address>/` was the assumption. On a stock
Dokku host it cannot work.

Dokku's own `00-default-vhost.conf` holds `listen 443 ssl default_server` with
`ssl_reject_handshake on`. A browser opening an address literal sends no SNI, so
nginx has no name to route on, falls to that port's default server, and the
connection is refused during the handshake, before any Host header is read.
Measured in the development environment — Ubuntu 24.04, Dokku 0.38.7, nginx
1.24, linux/arm64:

| What was run | What happened |
|---|---|
| `curl` to `https://<address>/`, with dokkup's server block on 443 | exit 35, refused in the TLS handshake |
| the same block reached by a name the client does send as SNI | 200 |
| `nginx -t` with `default_server` claimed for 443 in dokkup's file | fails — `a duplicate default server for 0.0.0.0:443` |
| `curl` to `https://<address>:8443/` | 200, with the certificate's IP SAN validating |

The third row is why the obvious fix is not one, and editing a file dokkup did
not create is not dokkup's to do — that is ADR-0008's promise, and it covers
Dokku's own vhost as much as its plugins. A port with exactly one server block
is that port's default server, so 8443 needs no SNI and conflicts with nothing.
The cost is a port number in the URL, which is honest for a mode this ADR
already calls degraded.

Declining the certificate is unchanged: plain HTTP stays at `http://<address>/`
on port 80, where nginx routes on the Host header and Dokku's catch-all claims
only the names nothing else does, so an address literal wins there. Measured
working.
