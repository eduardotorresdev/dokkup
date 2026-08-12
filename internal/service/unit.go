package service

import (
	"fmt"
	"strings"

	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
)

// DefaultListen is the address dokkup serves on unless told otherwise. It is
// loopback because Dokku's nginx is what faces the network (ADR-0006).
//
// It lives here rather than beside the serve flag because the unit is what
// actually decides how the service runs, and `dokkup update` reads the address
// back out of the installed unit to know where to look for health.
const DefaultListen = "127.0.0.1:8080"

// DefaultMode matches the server's own default: until dokkup has been published
// at a domain, it is reached by IP and restricts itself accordingly.
const DefaultMode = "ip"

// UnitConfig is what the unit needs to know about this host.
type UnitConfig struct {
	// Listen is the address the server binds. Empty means [DefaultListen].
	Listen string

	// Mode is how dokkup is reachable, "published" or "ip". Empty means
	// [DefaultMode].
	Mode string

	// Domain is the name dokkup answers to, when it has one.
	Domain string

	// PlainHTTP says this host is reached over plain HTTP, which the service
	// has to be told rather than left to guess: nginx terminates in front of
	// it, so the server sees an ordinary HTTP request either way and cannot
	// tell the two apart. What turns on it is the Secure flag on the session
	// cookie -- a browser drops a Secure cookie on an insecure origin, so
	// getting this wrong makes signing in fail silently, on the operator's
	// machine, with nothing in any log to say why.
	PlainHTTP bool

	// ManageCertificate asks the service to obtain and renew the certificate
	// for Domain over ACME.
	//
	// It is separate from Domain because an operator who supplied their own
	// certificate has a domain too, and renewing over the top of a certificate
	// somebody else issued -- an internal CA, a wildcard bought for the year --
	// would replace what they chose with something they did not ask for.
	ManageCertificate bool

	// ACMEEmail is the contact a certificate authority warns about expiry at.
	ACMEEmail string

	// ACMEDirectory points at a certificate authority other than Let's Encrypt.
	// It exists so that a test can issue against one it runs itself.
	ACMEDirectory string
}

// UnitFile renders the systemd unit.
//
// It is the single definition of how dokkup runs, so that installation, update
// and removal cannot each have their own idea of it. The hardening is defence
// in depth per ADR-0005: an authenticated operator is root-equivalent by
// design, and what these settings buy is that a flaw in the HTTP layer yields
// the surface of one binary rather than the whole host.
//
// [hostpaths.DataDir] must exist before the unit starts. systemd refuses to
// enter its mount namespace when a ReadWritePaths entry is missing, so the
// installer creates the directory rather than leaving systemd to fail at boot.
//
// Three of the directives ADR-0005 originally listed are deliberately absent,
// because dokkup's work is done by a program it executes and every sandbox here
// is inherited by that program. Each was measured against a real Dokku rather
// than reasoned about, and each failure is silent -- `dokku version`, the only
// command /api/health runs, keeps succeeding while the rest is broken:
//
//   - NoNewPrivileges=yes blocks setuid, so the sudo the dokku binary re-executes
//     itself through fails outright: `sudo: The "no new privileges" flag is set`.
//     dokkup never reaches Dokku at all.
//
//   - ProtectHome=yes hides /home/dokku, which is DOKKU_ROOT. Dokku then reads an
//     empty state root and answers truthfully about it: `apps:list` reports no
//     applications on a host that has them.
//
//   - ProtectSystem=strict makes /var/lib/dokku read-only, so reads work and
//     writes do not: `apps:create` fails with `read-only file system`.
//
// ProtectSystem=full is what is left, and it is the honest line: it protects
// what dokkup can name -- /usr, /boot, /etc, which includes its own binary --
// and leaves /var to Dokku, whose writable paths are not dokkup's to enumerate
// and would drift as Dokku changes. ADR-0005 calls this defence in depth rather
// than a security boundary, and the same ADR makes reaching Dokku the reason the
// service exists, so where the two collide the hardening yields. See ADR-0012.
func UnitFile(cfg UnitConfig) string {
	listen := cfg.Listen
	if listen == "" {
		listen = DefaultListen
	}
	mode := cfg.Mode
	if mode == "" {
		mode = DefaultMode
	}

	// Appended rather than always present, so that the ExecStart of an
	// installation that has no domain says so by saying nothing, and `systemctl
	// cat dokkup` stays readable as the record of how this host was set up.
	var extra strings.Builder
	if cfg.Domain != "" {
		fmt.Fprintf(&extra, " --domain %s", cfg.Domain)
	}
	if cfg.ManageCertificate {
		extra.WriteString(" --manage-certificate")
	}
	if cfg.ACMEEmail != "" {
		fmt.Fprintf(&extra, " --acme-email %s", cfg.ACMEEmail)
	}
	if cfg.ACMEDirectory != "" {
		fmt.Fprintf(&extra, " --acme-directory %s", cfg.ACMEDirectory)
	}
	if cfg.PlainHTTP {
		extra.WriteString(" --plain-http")
	}

	return fmt.Sprintf(`[Unit]
Description=dokkup -- a web interface for a Dokku host
Documentation=https://github.com/eduardotorresdev/dokkup

# Dokku does its work through Docker, and dokkup's first request usually asks
# Dokku something. Starting after both means a reboot does not present a
# degraded interface to whoever is watching it come back.
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=exec
ExecStart=%s serve --listen %s --mode %s%s
User=%s
Group=%s
Restart=on-failure
RestartSec=2s

# Defence in depth per ADR-0005, not a security boundary. NoNewPrivileges,
# ProtectHome and ProtectSystem=strict are absent on purpose: each one is
# inherited by the dokku process this service runs, and each one breaks it
# silently. See the doc comment on UnitFile, and ADR-0012.
ProtectSystem=full
PrivateTmp=yes
ReadWritePaths=%s

[Install]
WantedBy=multi-user.target
`,
		hostpaths.Binary, listen, mode, extra.String(),
		hostpaths.User, hostpaths.Group,
		hostpaths.DataDir,
	)
}

// ListenFromUnit reads the address out of a unit's ExecStart line.
//
// The unit is the authority on how the service runs, so this is how `dokkup
// update` finds the health endpoint of a service it did not start. It reports
// false when the unit says nothing about an address, which leaves the caller to
// fall back to [DefaultListen] -- the same default serve would have used.
func ListenFromUnit(unit string) (string, bool) {
	for line := range strings.SplitSeq(unit, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}

		fields := strings.Fields(strings.TrimPrefix(line, "ExecStart="))
		for i, field := range fields {
			if after, ok := strings.CutPrefix(field, "--listen="); ok {
				return after, after != ""
			}
			if field == "--listen" && i+1 < len(fields) {
				return fields[i+1], true
			}
		}
	}
	return "", false
}
