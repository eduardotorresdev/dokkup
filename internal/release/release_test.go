package release_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/release"
)

// Versions arrive in two shapes: goreleaser stamps "0.1.0" into a released
// binary, the Makefile stamps "v0.1.0" or a git describe into a local one.
// Comparing them as strings would make a released binary look different from
// itself, and offer an update that is already installed.
func TestVersionsAreComparedAsNumbersWhateverShapeTheyArriveIn(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		want    string
		running string
		newer   bool
		same    bool
	}{
		"a later patch":                    {want: "0.1.1", running: "0.1.0", newer: true},
		"a later minor":                    {want: "0.2.0", running: "0.1.9", newer: true},
		"a later major":                    {want: "1.0.0", running: "0.9.9", newer: true},
		"the same version":                 {want: "0.1.0", running: "0.1.0", same: true},
		"the same version, one v-prefixed": {want: "v0.1.0", running: "0.1.0", same: true},
		"an older version":                 {want: "0.1.0", running: "0.2.0"},
		// The one a string comparison gets wrong, and the first one a real
		// project hits.
		"ten is after nine": {want: "0.10.0", running: "0.9.0", newer: true},
		// A development build should be told an update exists rather than left
		// believing it is current.
		"anything beats a development build": {want: "0.1.0", running: "dev", newer: true},
		"a development build never wins":     {want: "dev", running: "0.1.0"},
		// git describe on a commit after a tag. It is still that tag's code plus
		// changes, so the released version is not newer than it.
		"a build after a tag":  {want: "0.1.0", running: "v0.1.0-3-gabc1234", same: true},
		"a prerelease of same": {want: "0.2.0", running: "v0.2.0-rc1", same: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := release.Newer(tc.want, tc.running); got != tc.newer {
				t.Errorf("Newer(%q, %q) = %v, want %v", tc.want, tc.running, got, tc.newer)
			}
			if got := release.SameVersion(tc.want, tc.running); got != tc.same {
				t.Errorf("SameVersion(%q, %q) = %v, want %v", tc.want, tc.running, got, tc.same)
			}
		})
	}
}

func TestABuildFromAWorkingTreeIsNotAReleasedVersion(t *testing.T) {
	t.Parallel()

	for version, want := range map[string]bool{
		"0.1.0":            true,
		"v0.1.0":           true,
		"v1.2.3-rc1":       true,
		"dev":              false,
		"":                 false,
		"0.1":              false,
		"1.2.3.4":          false,
		"v0.1.0-3-gabc123": true,
		"not a version":    false,
	} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()

			if got := release.IsRelease(version); got != want {
				t.Errorf("IsRelease(%q) = %v, want %v", version, got, want)
			}
		})
	}
}

func TestTheNewestReleaseIsWhateverGitHubCallsLatest(t *testing.T) {
	t.Parallel()

	github := &fakeGitHub{tag: "v1.4.2"}
	source := github.start(t)

	rel, err := source.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Tag != "v1.4.2" {
		t.Errorf("tag = %q, want v1.4.2", rel.Tag)
	}
	// Version is what `dokkup version` prints, so it has to come back without
	// the v or every comparison against a running binary needs its own trim.
	if rel.Version() != "1.4.2" {
		t.Errorf("version = %q, want 1.4.2", rel.Version())
	}
}

func TestAPinnedVersionIsAcceptedWithOrWithoutItsV(t *testing.T) {
	t.Parallel()

	for _, asked := range []string{"v0.2.0", "0.2.0", " 0.2.0 "} {
		t.Run(asked, func(t *testing.T) {
			t.Parallel()

			github := &fakeGitHub{tag: "v0.2.0"}
			source := github.start(t)

			rel, err := source.Resolve(context.Background(), asked)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", asked, err)
			}
			if rel.Tag != "v0.2.0" {
				t.Errorf("tag = %q, want v0.2.0", rel.Tag)
			}
		})
	}
}

// The tag goes into a URL path. Anything that is not a version is refused
// before a request is made, rather than sent to GitHub to see what happens.
func TestAVersionThatIsNotAVersionIsRefusedWithoutAskingGitHub(t *testing.T) {
	t.Parallel()

	for _, asked := range []string{
		"",
		"latest",
		"v0.1",
		"../../etc/passwd",
		"v0.1.0/../../../releases/tags/v9.9.9",
		"v0.1.0 --flag",
	} {
		t.Run(asked, func(t *testing.T) {
			t.Parallel()

			github := &fakeGitHub{}
			source := github.start(t)

			if _, err := source.Resolve(context.Background(), asked); err == nil {
				t.Fatalf("Resolve(%q) was accepted", asked)
			}
			if asked := github.asked(); len(asked) != 0 {
				t.Errorf("a request was made anyway: %v", asked)
			}
		})
	}
}

func TestAVersionThatWasNeverPublishedIsReportedAsMissingRatherThanAsAFailure(t *testing.T) {
	t.Parallel()

	github := &fakeGitHub{tag: "v0.2.0"}
	source := github.start(t)

	_, err := source.Resolve(context.Background(), "v9.9.9")
	if !errors.Is(err, release.ErrNoRelease) {
		t.Fatalf("error = %v, want it to wrap ErrNoRelease", err)
	}
	if !strings.Contains(err.Error(), "v9.9.9") {
		t.Errorf("the error does not say which version: %v", err)
	}
}

// Before the first release exists, /releases/latest is a 404. An operator who
// runs --check then deserves "nothing published yet", not a stack of HTTP.
func TestAnEmptyReleasesPageSaysSoPlainly(t *testing.T) {
	t.Parallel()

	github := &fakeGitHub{missing: map[string]bool{"release": true}}
	source := github.start(t)

	_, err := source.Latest(context.Background())
	if !errors.Is(err, release.ErrNoRelease) {
		t.Fatalf("error = %v, want it to wrap ErrNoRelease", err)
	}
}

func TestADraftReleaseIsNotOfferedAsAnUpdate(t *testing.T) {
	t.Parallel()

	github := &fakeGitHub{draft: true}
	source := github.start(t)

	if _, err := source.Latest(context.Background()); !errors.Is(err, release.ErrNoRelease) {
		t.Fatalf("error = %v, want it to wrap ErrNoRelease -- a draft has no assets to download", err)
	}
}
