// Package proxy owns how dokkup is reached through Dokku's nginx.
//
// Dokku's nginx already holds ports 80 and 443, so dokkup is not an app and
// cannot be one (ADR-0001). What it has instead is one file in conf.d, and the
// property worth having is that deleting that one file and reloading removes
// dokkup from the proxy completely: no symlink farm, no sites-available entry,
// no Dokku property to unset. Measured -- after removing the file and
// reloading, `grep -rl` for the name matched nothing anywhere under /etc/nginx
// and the catch-all answered instead, curl exit 35 on 443 and exit 52 on 80.
//
// The package holds text and keys and nothing else. It runs no program:
// whoever installs what it renders is who runs `nginx -t` and reloads, so that
// every process dokkup spawns is still findable in one file (ADR-0012).
//
// See docs/adr/0006-publishing-tls-and-ip-mode.md.
package proxy

import (
	"fmt"

	"github.com/eduardotorresdev/dokkup/internal/acme"
	"github.com/eduardotorresdev/dokkup/internal/service"
)

// DefaultTLSPort is where dokkup serves HTTPS in IP mode.
//
// Not 443. Dokku's own 00-default-vhost.conf owns `listen 443 ssl
// default_server` with `ssl_reject_handshake on`, and a browser opening
// https://<ip>/ sends no SNI, so it lands there and the handshake is refused
// before any Host header is read: measured, curl exit 35, while the very same
// vhost reached through a name the client does send as SNI answered 200.
// Claiming default_server for 443 fails `nginx -t` outright with "a duplicate
// default server for 0.0.0.0:443", and editing a file dokkup did not create is
// not dokkup's to do (ADR-0008).
//
// A port with exactly one server block IS that port's default server, so it
// needs no SNI and conflicts with nothing: measured working, with the
// IP-SAN certificate of [SelfSigned] validating against it. The cost is a port
// number in the URL an operator has to type, which is honest for a mode
// ADR-0006 already calls degraded.
const DefaultTLSPort = 8443

// Where the vhost looks for what it serves.
//
// These are the same two strings as hostpaths.TLSCert and hostpaths.TLSKey, and
// spelling them again is a real cost: hostpaths exists so that there is exactly
// one list of what dokkup puts on a host. It is paid because this package
// renders text and generates keys and depends on nothing that knows a host's
// layout, and because the drift it risks is caught in one assertion rather than
// on someone's server -- see
// [TestTheVhostNamesTheSamePlacesInstallationActuallyWritesTo].
//
// The paths themselves are fixed rather than chosen. The vhost is written once
// and any later renewal replaces the contents of these files rather than
// rewriting it, which is the only reason renewal from the running service is
// possible at all: under ProtectSystem=full the service cannot write /etc/nginx
// and can write /var/lib. Moving either name is a compatibility change for
// every installation that already exists, not an edit.
const (
	certPath = "/var/lib/dokkup/tls/cert.pem"
	keyPath  = "/var/lib/dokkup/tls/key.pem"
)

// Reachability is how a vhost presents dokkup.
type Reachability int

const (
	// AtDomain serves HTTPS at a domain with a certificate, redirecting HTTP.
	AtDomain Reachability = iota

	// AtIPWithTLS serves HTTPS at [DefaultTLSPort] with a self-signed
	// certificate.
	AtIPWithTLS

	// AtIPPlain serves HTTP on port 80 for the host's IP literal.
	//
	// Port 80 routes on the Host header, and Dokku's 00-default-vhost.conf
	// catch-all claims only the names nothing else does, so an IP-literal
	// server_name beats it: measured, a plain GET to the host's own address
	// reached the backend with r.Host set to that address, where the same
	// request with no matching server_name gets `return 444` and curl exit 52.
	//
	// `server_name _;` would not do here. The underscore is not a wildcard, it
	// is a name no Host header can equal, so on a port dokkup does not own the
	// default server the request falls through to Dokku's catch-all. On
	// [DefaultTLSPort] dokkup is the only server block and therefore already
	// that port's default server, which is why the TLS form can use it.
	AtIPPlain
)

// VhostConfig is what the vhost needs to know about this host.
type VhostConfig struct {
	// How is the form to render. The zero value is [AtDomain].
	How Reachability

	// Domain is the name dokkup answers to, for [AtDomain].
	Domain string

	// IP is the host's own address, for [AtIPPlain]. It is not in the
	// [AtIPWithTLS] form at all: that one is its port's default server and
	// answers whatever arrives, which is what makes it work without SNI.
	IP string

	// Port is where [AtIPWithTLS] listens. Zero means [DefaultTLSPort].
	Port int

	// Listen is the upstream, the address the service binds. Empty means
	// [service.DefaultListen].
	Listen string
}

// vhostHeader opens every form.
//
// The map is emitted even by the plain-HTTP form, because the location block
// that reads the variable is the same in all three and a form that dropped the
// map would fail `nginx -t` on an undefined variable. Two files defining the
// same map variable are tolerated -- measured, nginx -t exits 0 -- so the
// $dokkup_ prefix guards against nothing that would otherwise break; it is
// there so that a reader of /etc/nginx can tell whose variable it is.
const vhostHeader = `# /etc/nginx/conf.d/dokkup.conf -- written by dokkup.
#
# Deleting this file and reloading nginx removes dokkup from the proxy entirely.
# dokkup writes nothing else anywhere under /etc/nginx.

map $http_upgrade $dokkup_connection_upgrade {
    default upgrade;
    ""      close;
}
`

// domainServers is the published form: the redirect, then the TLS server.
//
// The port-80 block is not only a redirect. It also carries the one path a
// certificate authority fetches to prove this host answers for the name, and
// that path must not be redirected: an HTTP-01 validation follows no scheme
// change, so a block that sent everything to https:// would fail every
// validation -- including the first one, when the only certificate on disk is
// the self-signed placeholder the authority would then refuse.
//
// `^~` rather than a plain prefix. Longest-prefix already wins between these
// two, so the marker changes nothing today; it is there because it also stops
// any regex location a later version adds from silently taking the path back,
// and a certificate that stops renewing fails ninety days after the change that
// broke it.
const domainServers = `
server {
    listen      80;
    listen      [::]:80;
    server_name %[1]s;

    location ^~ %[4]s {
        proxy_pass http://%[5]s;
        proxy_set_header Host $http_host;
    }

    location / { return 301 https://$host$request_uri; }
}

server {
    listen      443 ssl;
    listen      [::]:443 ssl;
    server_name %[1]s;

    ssl_certificate     %[2]s;
    ssl_certificate_key %[3]s;
    ssl_protocols       TLSv1.2 TLSv1.3;

`

// ipTLSServer is IP mode with a certificate. There is no port 80 block: a
// redirect to a self-signed https:// URL would send the operator's browser to a
// warning page it had no reason to expect, and the plain-HTTP form exists for
// operators who did not want a certificate.
const ipTLSServer = `
server {
    listen      %[1]d ssl;
    listen      [::]:%[1]d ssl;
    server_name _;

    ssl_certificate     %[2]s;
    ssl_certificate_key %[3]s;
    ssl_protocols       TLSv1.2 TLSv1.3;

`

// ipPlainServer is IP mode without a certificate. It must not redirect: there
// is nowhere to redirect to, and a vhost that sent every request to https://
// would make dokkup unreachable rather than merely unencrypted.
const ipPlainServer = `
server {
    listen      80;
    listen      [::]:80;
    server_name %s;

`

// proxyBody closes every server block. The limit and the location are the same
// whichever way dokkup is reached, so that what a request may carry does not
// depend on how the operator got here; nginx's own default is 1m. The
// decisions inside the location are recorded on [VhostFile].
const proxyBody = `    client_max_body_size 10m;

    location / {
        proxy_pass http://%s;

        proxy_http_version 1.1;
        proxy_set_header Host              $http_host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Port  $server_port;
        proxy_set_header Upgrade           $http_upgrade;
        proxy_set_header Connection        $dokkup_connection_upgrade;

        proxy_buffering off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
`

// VhostFile renders dokkup's one nginx file.
//
// Four things in it were settled against a real nginx 1.24 on Ubuntu 24.04
// rather than chosen, and each of the four is invisible until the day it is
// not:
//
//   - There is no http2 directive, in any form. `http2 on;` does not exist on
//     nginx 1.24, which is what Ubuntu 24.04 ships and therefore what a Dokku
//     host is running: `[emerg] unknown directive "http2"` and `nginx -t` exits
//     1, which under nginx.service's ExecStartPre is a refusal to start rather
//     than a failed reload. The older `listen ... ssl http2` spelling is
//     deprecated with a warning from 1.25.1, so no single spelling works on
//     both. Dokku's own template branches on the version; omitting HTTP/2
//     entirely removes that branch, and an admin interface gains nothing from
//     it that a reader would notice.
//
//   - Host is $http_host and not $host, because $http_host is what the client
//     actually sent and keeps the port with it. $host drops the port, and IP
//     mode is reached at an explicit one, so dokkup would build its own URLs
//     without the port it is being reached on.
//
//   - proxy_http_version 1.1 and the Upgrade/Connection pair are here from the
//     first installation rather than added when log streaming lands (#18).
//     Without the version line the upstream request is HTTP/1.0 -- measured,
//     the backend reported `proto=HTTP/1.0` -- and HTTP/1.0 cannot carry an
//     upgrade at all. With this block exactly as written, measured: `HTTP/1.1
//     101 Switching Protocols` with `Sec-WebSocket-Accept:
//     FB5PkYy40E2+nBai53zk8l8SG9I=`, the frame delivered, and X-Forwarded-For
//     preserved across the upgrade. The cost of carrying them early is three
//     lines nobody exercises yet; the cost of adding them later is rewriting
//     the vhost of every installation that already exists, and rewriting a
//     vhost is the one operation that can leave a host unable to reach dokkup.
//
//   - There are no access_log or error_log paths, so that removal stays exactly
//     one file. A log path of dokkup's own would be a second thing to remove
//     and a second thing to forget.
//
// proxy_buffering off and the 3600s timeouts are for streamed logs too, and
// cost nothing until there are any.
func VhostFile(cfg VhostConfig) string {
	listen := cfg.Listen
	if listen == "" {
		listen = service.DefaultListen
	}

	var servers string
	switch cfg.How {
	case AtIPWithTLS:
		port := cfg.Port
		if port == 0 {
			port = DefaultTLSPort
		}
		servers = fmt.Sprintf(ipTLSServer, port, certPath, keyPath)
	case AtIPPlain:
		servers = fmt.Sprintf(ipPlainServer, cfg.IP)
	default:
		// AtDomain is the zero value, so it is also the branch a Reachability
		// nobody defined lands in. That renders the published form with an
		// empty server_name, which `nginx -t` refuses -- which is the right way
		// round, because the alternative is a file nginx accepts and that
		// serves nothing.
		servers = fmt.Sprintf(domainServers, cfg.Domain, certPath, keyPath,
			acme.ChallengePath, listen)
	}

	return vhostHeader + servers + fmt.Sprintf(proxyBody, listen)
}
