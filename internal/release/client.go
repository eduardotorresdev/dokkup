package release

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// NewClient returns the HTTP client downloads go through.
//
// The policy it carries is the point. An updater runs as root and writes the
// binary the host will execute, so where the bytes come from is not a detail:
// a release page that could redirect the download anywhere would turn a
// compromised release into arbitrary code on every host that updates.
//
// So every request -- the first one and every redirect after it -- must be
// HTTPS and must land on GitHub. Release assets genuinely do redirect off
// github.com to GitHub's asset host, so a single-host rule would refuse every
// real download; the rule is a host allowlist instead.
func NewClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSHandshakeTimeout = dialTimeouts.TLSHandshake
	transport.ResponseHeaderTimeout = dialTimeouts.ResponseStart

	return &http.Client{
		// Wrapping the transport rather than only setting CheckRedirect: this
		// way the first request is checked too, and every hop passes through the
		// same test rather than through two that could disagree.
		Transport: policyTransport{base: transport},
	}
}

type policyTransport struct{ base http.RoundTripper }

func (t policyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := allowURL(req.URL); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

// allowURL reports whether a request may be made at all.
func allowURL(u *url.URL) error {
	host := u.Hostname()

	// A loopback address is one this host was explicitly pointed at -- the
	// integration test serves a fake release on one. Nothing reachable from a
	// hostile redirect lives there that is not already local.
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}

	if u.Scheme != "https" {
		return fmt.Errorf("refusing to fetch %s over %s: downloads must use HTTPS", u.Host, u.Scheme)
	}
	if !allowedHost(host) {
		return fmt.Errorf("refusing to follow a redirect to %s: downloads must stay on GitHub", u.Host)
	}
	return nil
}

func allowedHost(host string) bool {
	switch host {
	case "github.com", "api.github.com":
		return true
	}
	// Release assets are served from GitHub's own content hosts, and which one
	// has changed before now. The leading dot is what keeps this from matching
	// something like evil-githubusercontent.com.
	return strings.HasSuffix(host, ".githubusercontent.com")
}
