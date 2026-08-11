package release

import (
	"net/url"
	"strings"
	"testing"
)

// The updater runs as root and writes the binary this host will execute, so
// where the bytes are allowed to come from is a security decision rather than a
// configuration one. These are the cases that decision has to get right.
func TestDownloadsAreConfinedToGitHubOverHTTPS(t *testing.T) {
	t.Parallel()

	for raw, allowed := range map[string]bool{
		"https://github.com/eduardotorresdev/dokkup/releases/download/v0.1.0/dokkup_linux_amd64": true,
		"https://api.github.com/repos/eduardotorresdev/dokkup/releases/latest":                   true,

		// Where release assets actually land after two redirects. Verified
		// against the real service: a github.com-only rule refuses every
		// download there is.
		"https://release-assets.githubusercontent.com/github-production-release-asset/1/2": true,
		"https://objects.githubusercontent.com/github-production-release-asset/1/2":        true,

		// Plain HTTP anywhere but loopback: a network attacker chooses the
		// bytes, and the only thing standing between that and root is a
		// checksum served over the same channel.
		"http://github.com/eduardotorresdev/dokkup/releases/latest": false,

		// The redirect a compromised release page would use.
		"https://example.com/dokkup_linux_amd64":  false,
		"https://github.com.example.com/x":        false,
		"https://evil-githubusercontent.com/x":    false,
		"https://githubusercontent.com.evil.io/x": false,
		"https://raw.githubusercontent.com/x":     true,

		// The integration test serves a release from a local file server, and
		// nothing hostile can be reached there that is not already on the host.
		"http://127.0.0.1:8000/releases/download/v0.1.0/dokkup_linux_amd64": true,
		"http://localhost:8000/releases/latest":                             true,
		"http://[::1]:8000/releases/latest":                                 true,

		// Not loopback, whatever it looks like.
		"http://127.0.0.1.example.com/x": false,
		"http://10.0.0.1/x":              false,
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			parsed, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parsing %q: %v", raw, err)
			}

			err = allowURL(parsed)
			if allowed && err != nil {
				t.Errorf("refused a legitimate URL: %v", err)
			}
			if !allowed && err == nil {
				t.Error("allowed a URL that downloads must never follow")
			}
		})
	}
}

// A released binary must never carry a base URL that points somewhere else.
// The integration test overrides these with -ldflags, so this is what keeps an
// override from surviving into something published.
func TestTheCompiledInEndpointsAreTheRealOnesOverHTTPS(t *testing.T) {
	t.Parallel()

	for name, got := range map[string]string{
		"base": defaultBaseURL,
		"api":  defaultAPIURL,
	} {
		if !strings.HasPrefix(got, "https://") {
			t.Errorf("the %s endpoint is not HTTPS: %s", name, got)
		}
		if !strings.Contains(got, Repo) {
			t.Errorf("the %s endpoint does not point at %s: %s", name, Repo, got)
		}

		parsed, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parsing the %s endpoint: %v", name, err)
		}
		if err := allowURL(parsed); err != nil {
			t.Errorf("the %s endpoint is one its own client would refuse: %v", name, err)
		}
	}
}
