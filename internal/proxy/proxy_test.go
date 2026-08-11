package proxy_test

import (
	"strings"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/acme"
	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/proxy"
	"github.com/eduardotorresdev/dokkup/internal/service"
)

// everyForm is each way a vhost can present dokkup, so that a property is
// asserted of all three rather than of whichever one the author had in mind.
// The forms differ in how they are reached and in nothing else, and most of
// what can go wrong here goes wrong in the one nobody was thinking about.
var everyForm = map[string]proxy.VhostConfig{
	"published at a domain": {How: proxy.AtDomain, Domain: "dokkup.example.com"},
	"by IP with TLS":        {How: proxy.AtIPWithTLS, IP: "192.0.2.7"},
	"by IP over plain HTTP": {How: proxy.AtIPPlain, IP: "192.0.2.7"},
}

// Without proxy_http_version 1.1 the upstream request is HTTP/1.0, which cannot
// carry an upgrade at all, so this is not a feature waiting to be switched on
// later: it is three lines that have to be in the file the first time it is
// written. Adding them afterwards means rewriting the vhost of every host
// already installed, which is the one operation that can leave dokkup
// unreachable.
func TestTheVhostCarriesTheUpgradeHeadersItWillNeedForLogs(t *testing.T) {
	t.Parallel()

	for name, cfg := range everyForm {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			vhost := proxy.VhostFile(cfg)

			for _, want := range []string{
				"map $http_upgrade $dokkup_connection_upgrade {",
				"proxy_http_version 1.1;",
				"proxy_set_header Upgrade           $http_upgrade;",
				"proxy_set_header Connection        $dokkup_connection_upgrade;",
			} {
				if !strings.Contains(vhost, want) {
					t.Errorf("the vhost is missing %q:\n%s", want, vhost)
				}
			}
		})
	}
}

// `http2 on;` does not exist on nginx 1.24, which is what Ubuntu 24.04 ships:
// it is an [emerg] unknown directive and `nginx -t` exits 1. Because
// nginx.service runs `nginx -t -q` as ExecStartPre, a file carrying it is not a
// failed reload but a refusal to start, which takes every app on the host down
// at the next reboot.
func TestTheVhostAsksForNoHTTP2Directive(t *testing.T) {
	t.Parallel()

	for name, cfg := range everyForm {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if vhost := proxy.VhostFile(cfg); strings.Contains(vhost, "http2") {
				t.Errorf("the vhost asks for HTTP/2, which nginx 1.24 refuses:\n%s", vhost)
			}
		})
	}
}

// Dokku's 00-default-vhost.conf owns `listen 443 ssl default_server` with
// ssl_reject_handshake on. An IP literal sends no SNI, so it lands there and is
// refused at handshake (curl exit 35), and claiming default_server for 443
// fails nginx -t with "a duplicate default server for 0.0.0.0:443". Neither is
// dokkup's to fix, so IP mode stays off 443 entirely.
func TestIPModeDoesNotTryToOwnPort443(t *testing.T) {
	t.Parallel()

	for name, cfg := range everyForm {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			vhost := proxy.VhostFile(cfg)

			// No form claims a port's default server. On 443 nginx refuses the
			// whole configuration; on the dedicated TLS port there is nothing
			// to claim, because a port with one server block already is that
			// port's default server.
			if strings.Contains(vhost, "default_server") {
				t.Errorf("the vhost claims a default server:\n%s", vhost)
			}

			if cfg.How == proxy.AtDomain {
				return
			}

			// Not a bare "443": the TLS form listens on 8443 and would match it.
			for _, banned := range []string{"listen      443", "[::]:443"} {
				if strings.Contains(vhost, banned) {
					t.Errorf("an IP-mode vhost listens on 443 (%q):\n%s", banned, vhost)
				}
			}
		})
	}
}

func TestTheTLSPortIsTheOneNothingElseOnADokkuHostAlreadyOwns(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		port int
		want string
	}{
		"left to dokkup":         {port: 0, want: "listen      8443 ssl;"},
		"chosen by the operator": {port: 9443, want: "listen      9443 ssl;"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			vhost := proxy.VhostFile(proxy.VhostConfig{
				How:  proxy.AtIPWithTLS,
				IP:   "192.0.2.7",
				Port: tc.port,
			})

			if !strings.Contains(vhost, tc.want) {
				t.Errorf("the vhost does not carry %q:\n%s", tc.want, vhost)
			}
		})
	}
}

// The upstream has to be the address the unit actually binds. They are set from
// the same flag at install time, and a vhost pointing at a port nothing is
// listening on produces a 502 that looks like dokkup crashed.
func TestEveryFormProxiesToTheAddressTheServiceIsBoundTo(t *testing.T) {
	t.Parallel()

	for name, cfg := range everyForm {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if vhost := proxy.VhostFile(cfg); !strings.Contains(vhost, "proxy_pass http://"+service.DefaultListen+";") {
				t.Errorf("the vhost does not proxy to the default address:\n%s", vhost)
			}

			cfg.Listen = "127.0.0.1:9999"
			if vhost := proxy.VhostFile(cfg); !strings.Contains(vhost, "proxy_pass http://127.0.0.1:9999;") {
				t.Errorf("the vhost ignored the address it was given:\n%s", vhost)
			}
		})
	}
}

// $host drops the port; $http_host is what the client sent, port and all. IP
// mode is reached at an explicit port, so with $host dokkup would build its own
// URLs without the port it is being reached on.
func TestTheHostHeaderGoingUpstreamKeepsThePortItArrivedOn(t *testing.T) {
	t.Parallel()

	for name, cfg := range everyForm {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			vhost := proxy.VhostFile(cfg)

			var found bool
			for line := range strings.SplitSeq(vhost, "\n") {
				fields := strings.Fields(line)
				if len(fields) != 3 || fields[0] != "proxy_set_header" || fields[1] != "Host" {
					continue
				}
				found = true
				if fields[2] != "$http_host;" {
					t.Errorf("Host is set to %s, want $http_host;", fields[2])
				}
			}
			if !found {
				t.Errorf("the vhost passes no Host header upstream:\n%s", vhost)
			}
		})
	}
}

// Port 80 routes on the Host header, and Dokku's catch-all claims only the
// names nothing else does, so the IP literal is what beats it. `server_name _;`
// would lose: the underscore is a name no Host header can equal, so the request
// would fall through to Dokku's `return 444`.
func TestThePlainHTTPFormAnswersToTheIPLiteralRatherThanToNothing(t *testing.T) {
	t.Parallel()

	vhost := proxy.VhostFile(proxy.VhostConfig{How: proxy.AtIPPlain, IP: "192.0.2.7"})

	if !strings.Contains(vhost, "server_name 192.0.2.7;") {
		t.Errorf("the vhost does not answer to the host's own address:\n%s", vhost)
	}
	if strings.Contains(vhost, "server_name _;") {
		t.Errorf("the vhost answers to a name no request can carry:\n%s", vhost)
	}
}

// Only the published form has somewhere to send a plain request. An IP-mode
// vhost that redirected to https:// would make dokkup unreachable rather than
// merely unencrypted -- there is no listener on the other side of that redirect
// in the plain form, and a certificate nobody has accepted yet in the TLS one.
func TestOnlyThePublishedFormSendsAPlainRequestSomewhereElse(t *testing.T) {
	t.Parallel()

	for name, cfg := range everyForm {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			vhost := proxy.VhostFile(cfg)
			redirects := strings.Contains(vhost, "return 301 https://$host$request_uri;")

			if want := cfg.How == proxy.AtDomain; redirects != want {
				t.Errorf("redirects = %v, want %v:\n%s", redirects, want, vhost)
			}
		})
	}
}

// The one path that must not be redirected, in the one form that redirects.
//
// An HTTP-01 validation follows no scheme change, so a port-80 block that sent
// everything to https:// would fail every validation -- including the first,
// when the only certificate on disk is the self-signed placeholder the authority
// would then refuse. A certificate that stops renewing fails ninety days after
// the change that broke it, which is why this is asserted rather than left to
// the day somebody notices.
func TestThePublishedFormLetsTheChallengeThroughInsteadOfRedirectingIt(t *testing.T) {
	t.Parallel()

	vhost := proxy.VhostFile(proxy.VhostConfig{How: proxy.AtDomain, Domain: "dokkup.example.com"})

	location := "location ^~ " + acme.ChallengePath
	if !strings.Contains(vhost, location) {
		t.Fatalf("the vhost has no %q:\n%s", location, vhost)
	}

	// Ahead of the redirect, and marked ^~ so that no regex location a later
	// version adds can silently take the path back.
	challenge := strings.Index(vhost, location)
	redirect := strings.Index(vhost, "return 301 https://")
	if redirect < challenge {
		t.Errorf("the redirect is written before the challenge location:\n%s", vhost)
	}

	// And it goes to dokkup rather than to a directory on disk: nginx's workers
	// run as www-data on a Dokku host, and dokkup's data directory is 0750
	// dokkup:dokkup, so a webroot would be unreadable by the process serving it.
	block := vhost[challenge:]
	if end := strings.Index(block, "}"); end >= 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "proxy_pass http://"+service.DefaultListen) {
		t.Errorf("the challenge is not proxied to the service:\n%s", block)
	}
	if strings.Contains(block, "root ") || strings.Contains(block, "alias ") {
		t.Errorf("the challenge is served from a directory nginx's workers cannot read:\n%s", block)
	}
}

// The other forms have no port-80 name to be validated at, so the location is
// not merely unnecessary there -- in IP mode it would be a path answering on a
// host that can never hold a certificate for the address it is reached by.
func TestOnlyThePublishedFormCarriesTheChallengeLocation(t *testing.T) {
	t.Parallel()

	for name, cfg := range everyForm {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			vhost := proxy.VhostFile(cfg)
			carries := strings.Contains(vhost, acme.ChallengePath)

			if want := cfg.How == proxy.AtDomain; carries != want {
				t.Errorf("carries the challenge = %v, want %v:\n%s", carries, want, vhost)
			}
		})
	}
}

// The vhost spells its own paths, because internal/proxy renders text and
// generates keys and depends on nothing that knows a host's layout. That leaves
// hostpaths and this package holding the same three strings when hostpaths is
// meant to be the only one holding any of them, so this is the assertion that
// stops them drifting: a vhost pointing at a certificate installation does not
// write fails nginx -t on a real host and passes every other test in this file.
func TestTheVhostNamesTheSamePlacesInstallationActuallyWritesTo(t *testing.T) {
	t.Parallel()

	for name, cfg := range everyForm {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			vhost := proxy.VhostFile(cfg)

			// The file says where it is, because an operator reading
			// /etc/nginx should not have to work out which file this is.
			if want := "# " + hostpaths.Vhost + " -- written by dokkup."; !strings.Contains(vhost, want) {
				t.Errorf("the vhost does not name itself as %q:\n%s", want, vhost)
			}

			if cfg.How == proxy.AtIPPlain {
				return
			}
			for _, want := range []string{
				"ssl_certificate     " + hostpaths.TLSCert + ";",
				"ssl_certificate_key " + hostpaths.TLSKey + ";",
			} {
				if !strings.Contains(vhost, want) {
					t.Errorf("the vhost does not serve %q:\n%s", want, vhost)
				}
			}
		})
	}
}

// Removal is one os.Remove, one nginx -t and one reload, and that is only true
// while the file names nothing outside itself. A log path of dokkup's own would
// be a second thing to remove and a second thing to forget.
func TestTheVhostNamesNoPlaceOtherThanTheOneFileAndTheCertificate(t *testing.T) {
	t.Parallel()

	for name, cfg := range everyForm {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			vhost := proxy.VhostFile(cfg)

			for _, banned := range []string{"access_log", "error_log", "include "} {
				if strings.Contains(vhost, banned) {
					t.Errorf("the vhost sets %q, so removing it is no longer one file:\n%s", banned, vhost)
				}
			}
		})
	}
}
