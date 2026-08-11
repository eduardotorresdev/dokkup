// Package release finds, downloads and verifies published dokkup releases.
//
// It is deliberately independent of the CLI. Telling an operator that a newer
// version exists is wanted in the web interface too, and there must not be two
// answers to "what is the newest version" or two ideas of what makes a download
// trustworthy.
//
// Nothing here writes to the installation. Verification either succeeds and
// leaves a checked binary in a directory the caller owns, or fails and leaves
// nothing behind.
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Repo is the GitHub repository releases come from. It is also half of the
// identity a signature is checked against, so it is not merely a URL fragment.
const Repo = "eduardotorresdev/dokkup"

// The compiled-in endpoints. They are variables rather than constants so that
// the integration test can point a throwaway build at a local server with
// -ldflags -X; a released binary never carries that flag, which is why
// [TestTheCompiledInEndpointsAreTheRealOnesOverHTTPS] guards their value.
var (
	defaultBaseURL = "https://github.com/" + Repo + "/releases"
	defaultAPIURL  = "https://api.github.com/repos/" + Repo
)

// ErrNoRelease reports that the repository has published nothing yet, or that
// the requested version does not exist.
var ErrNoRelease = errors.New("no such release")

// tagPattern is what a dokkup tag looks like. Resolving puts the tag in a URL
// path, so it is matched before use rather than trusted.
var tagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// Release is a published release.
type Release struct {
	// Tag is the git tag, with its leading v: "v0.1.0".
	Tag string

	// URL is the release page, for printing. Empty when unknown.
	URL string
}

// Version is the tag without its leading v, which is the form goreleaser
// stamps into the binary and therefore the form `dokkup version` prints.
func (r Release) Version() string { return strings.TrimPrefix(r.Tag, "v") }

// Source fetches dokkup releases from GitHub.
//
// The zero value works and talks to the real repository. The two URL fields are
// the seam tests use: pointing them at a local server exercises the download,
// the checksum and both signature paths without reaching the network.
type Source struct {
	// BaseURL is where release assets live. Empty means the compiled-in
	// default. Asset URLs are BaseURL + "/download/<tag>/<asset>".
	BaseURL string

	// APIURL is the repository's API root. Empty means the compiled-in default.
	APIURL string

	// HTTP is the client used. Empty means one built by [NewClient], which is
	// what enforces the transport policy; supplying another means taking that
	// enforcement on yourself.
	HTTP *http.Client

	// CosignBinary is the signature verifier. Empty means whatever "cosign" is
	// on PATH, and nothing when there is none.
	CosignBinary string

	// StallTimeout is how long a download may produce nothing before it is
	// abandoned. Zero means [stallTimeout].
	StallTimeout time.Duration
}

func (s *Source) baseURL() string {
	if s.BaseURL == "" {
		return defaultBaseURL
	}
	return strings.TrimSuffix(s.BaseURL, "/")
}

func (s *Source) apiURL() string {
	if s.APIURL == "" {
		return defaultAPIURL
	}
	return strings.TrimSuffix(s.APIURL, "/")
}

func (s *Source) stall() time.Duration {
	if s.StallTimeout > 0 {
		return s.StallTimeout
	}
	return stallTimeout
}

func (s *Source) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return NewClient()
}

// Latest returns the newest published release.
//
// GitHub's own definition is used: /releases/latest is the most recent release
// that is neither a draft nor a prerelease, which is the same thing
// `releases/latest/download` serves and therefore the same thing install.sh
// installs.
func (s *Source) Latest(ctx context.Context) (Release, error) {
	return s.release(ctx, s.apiURL()+"/releases/latest", "")
}

// Resolve returns the release for a specific tag. A bare "0.1.0" is accepted
// and read as "v0.1.0", because that is the form `dokkup version` prints.
func (s *Source) Resolve(ctx context.Context, tag string) (Release, error) {
	tag = strings.TrimSpace(tag)
	if tag != "" && !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	if !tagPattern.MatchString(tag) {
		return Release{}, fmt.Errorf("%q is not a version like v0.1.0", tag)
	}
	return s.release(ctx, s.apiURL()+"/releases/tags/"+tag, tag)
}

// release reads one release from the API. wantTag, when set, is checked against
// what came back, so a redirect or a misconfigured endpoint cannot quietly
// hand back a different version than the one asked for.
func (s *Source) release(ctx context.Context, url, wantTag string) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client().Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("asking GitHub for the release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		if wantTag != "" {
			return Release{}, fmt.Errorf("%w: %s", ErrNoRelease, wantTag)
		}
		return Release{}, fmt.Errorf("%w: nothing has been published yet", ErrNoRelease)
	case http.StatusForbidden, http.StatusTooManyRequests:
		// Unauthenticated callers get 60 requests an hour per address. Saying so
		// is the difference between "try later" and "something is broken".
		return Release{}, fmt.Errorf("GitHub is rate-limiting this host (%s); try again later", resp.Status)
	default:
		return Release{}, fmt.Errorf("asking GitHub for the release: %s", resp.Status)
	}

	var body struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Draft   bool   `json:"draft"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes)).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("reading GitHub's answer: %w", err)
	}

	if body.TagName == "" || body.Draft {
		return Release{}, fmt.Errorf("%w: GitHub returned no usable release", ErrNoRelease)
	}
	if wantTag != "" && body.TagName != wantTag {
		return Release{}, fmt.Errorf("asked for %s but GitHub answered with %s", wantTag, body.TagName)
	}
	if !tagPattern.MatchString(body.TagName) {
		return Release{}, fmt.Errorf("%q is not a version like v0.1.0", body.TagName)
	}

	return Release{Tag: body.TagName, URL: body.HTMLURL}, nil
}

// userAgent identifies dokkup to GitHub, which rejects API requests that send
// none at all.
const userAgent = "dokkup-update"

// maxMetadataBytes bounds the JSON read from the API. Release metadata is a few
// kilobytes; anything larger is not something to keep reading.
const maxMetadataBytes = 1 << 20

// Newer reports whether want is a later version than running.
//
// Both sides arrive in whatever shape stamped them: goreleaser writes "0.1.0",
// the Makefile writes "v0.1.0" or "dev". Comparison is on the numeric triple
// with any leading v and any prerelease suffix removed, so v0.2.0-rc1 and
// v0.2.0 compare equal -- close enough for deciding whether to offer an update,
// and dokkup does not publish prereleases.
//
// A version that parses as nothing at all is treated as older than every
// release: a development build should be told an update exists rather than
// being left to think it is current.
func Newer(want, running string) bool { return compare(want, running) > 0 }

// SameVersion reports whether two version strings name the same release.
func SameVersion(a, b string) bool { return compare(a, b) == 0 }

// IsRelease reports whether a version string names a published release rather
// than a build from a working tree.
func IsRelease(version string) bool {
	_, ok := parseVersion(version)
	return ok
}

func compare(a, b string) int {
	left, leftOK := parseVersion(a)
	right, rightOK := parseVersion(b)

	switch {
	case !leftOK && !rightOK:
		return 0
	case !leftOK:
		return -1
	case !rightOK:
		return 1
	}

	for i := range left {
		if left[i] != right[i] {
			if left[i] < right[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVersion(version string) ([3]int, bool) {
	version = strings.TrimSpace(version)
	version = strings.TrimPrefix(version, "v")
	if base, _, found := strings.Cut(version, "-"); found {
		version = base
	}
	// The Makefile stamps `git describe`, which appends the commit for a build
	// that is not exactly on a tag. That is still the tag it came after.
	if base, _, found := strings.Cut(version, "+"); found {
		version = base
	}

	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}

	var triple [3]int
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		triple[i] = n
	}
	return triple, true
}

// dialTimeouts keep a stalled connection from holding an update open before the
// response starts arriving. Once it has, [stallTimeout] takes over.
var dialTimeouts = struct {
	TLSHandshake  time.Duration
	ResponseStart time.Duration
}{10 * time.Second, 30 * time.Second}

// stallTimeout is how long a download may go without producing a single byte
// before it is abandoned.
//
// It bounds the transfer without putting a ceiling on its total length, which is
// the distinction that matters: a 25 MB binary over a slow link is slow rather
// than broken, and should finish, while a server that accepted the connection
// and then went quiet should not hold a root command open indefinitely.
const stallTimeout = 60 * time.Second
