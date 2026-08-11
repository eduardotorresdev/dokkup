package release_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/release"
)

func TestAVerifiedDownloadIsLeftExecutableAndByteForByteWhatWasPublished(t *testing.T) {
	t.Parallel()

	github := &fakeGitHub{binary: []byte("the published bytes")}
	source := github.start(t)
	cosign, _ := stubCosign(t, 0)
	source.CosignBinary = cosign

	rel, err := source.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}

	dir := t.TempDir()
	artifact, err := source.Fetch(context.Background(), rel, "amd64", dir)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if artifact.Name != "dokkup_linux_amd64" {
		t.Errorf("name = %q, want dokkup_linux_amd64", artifact.Name)
	}
	if !artifact.Signed {
		t.Error("Signed is false although cosign verified it")
	}

	content, err := os.ReadFile(artifact.Path)
	if err != nil {
		t.Fatalf("reading the artifact: %v", err)
	}
	if string(content) != "the published bytes" {
		t.Errorf("content = %q, want the published bytes", content)
	}

	info, err := os.Stat(artifact.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// systemd will execute this file. A download left at 0600 would install and
	// then fail to start, which is a much worse way to find that out.
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("mode = %04o, want 0755", perm)
	}
}

// install.sh makes exactly this trade, and dokkup must make the same one: the
// checksum proves the bytes are the published ones, and without cosign nothing
// proves who published them. Refusing outright would leave every host without
// cosign unable to update; saying nothing would overstate what was checked.
func TestWithoutCosignTheChecksumStillStandsAndTheResultSaysSoRatherThanRefusing(t *testing.T) {
	// Not parallel: it empties PATH, and t.Setenv forbids the two together.
	t.Setenv("PATH", t.TempDir())

	github := &fakeGitHub{binary: []byte("the published bytes")}
	source := github.start(t)

	artifact, err := source.Fetch(context.Background(), release.Release{Tag: github.tag}, "amd64", t.TempDir())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if artifact.Signed {
		t.Error("Signed is true although there was no cosign to verify with")
	}

	content, err := os.ReadFile(artifact.Path)
	if err != nil {
		t.Fatalf("reading the artifact: %v", err)
	}
	if string(content) != "the published bytes" {
		t.Errorf("content = %q, want the published bytes", content)
	}
}

func TestTheArchitectureDecidesTheAsset(t *testing.T) {
	t.Parallel()

	for goarch, want := range map[string]string{
		"amd64": "dokkup_linux_amd64",
		"arm64": "dokkup_linux_arm64",
	} {
		t.Run(goarch, func(t *testing.T) {
			t.Parallel()

			got, err := release.AssetName(goarch)
			if err != nil {
				t.Fatalf("AssetName(%q): %v", goarch, err)
			}
			if got != want {
				t.Errorf("asset = %q, want %q", got, want)
			}
		})
	}

	if _, err := release.AssetName("riscv64"); err == nil {
		t.Error("an architecture with no published binary was accepted")
	}
}

// Every one of these is a download that must not become an installed binary,
// and none of them may leave anything behind that a later run could mistake for
// a finished download.
func TestADownloadThatCannotBeVerifiedInstallsNothingAndLeavesNothing(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		github  *fakeGitHub
		cosign  int
		says    string
		noStub  bool
		wantErr string
	}{
		"the bytes do not match the checksum": {
			github: &fakeGitHub{
				binary: []byte("the published bytes"),
				sums:   strings.Repeat("a", 64) + "  dokkup_linux_amd64\n",
			},
			noStub:  true,
			wantErr: "checksum mismatch",
		},
		"the asset is not listed in SHA256SUMS": {
			github: &fakeGitHub{
				sums: strings.Repeat("a", 64) + "  something_else\n",
			},
			noStub:  true,
			wantErr: "does not list",
		},
		"SHA256SUMS is not there at all": {
			github:  &fakeGitHub{missing: map[string]bool{"SHA256SUMS": true}},
			noStub:  true,
			wantErr: "SHA256SUMS",
		},
		"the binary is not there at all": {
			github:  &fakeGitHub{missing: map[string]bool{"dokkup_linux_amd64": true}},
			noStub:  true,
			wantErr: "404",
		},
		"the signature does not verify": {
			github:  &fakeGitHub{},
			cosign:  1,
			wantErr: "signature verification failed",
		},
		"the signature is not published": {
			github:  &fakeGitHub{missing: map[string]bool{"dokkup_linux_amd64.cosign.bundle": true}},
			cosign:  0,
			wantErr: "downloading the signature",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			source := tc.github.start(t)
			if !tc.noStub {
				binary, _ := stubCosign(t, tc.cosign)
				source.CosignBinary = binary
			}
			// The noStub cases all fail before a signature is ever looked at,
			// so they behave the same whether or not this machine has cosign.

			dir := t.TempDir()
			rel := release.Release{Tag: tc.github.tag}

			_, err := source.Fetch(context.Background(), rel, "amd64", dir)
			if err == nil {
				t.Fatal("an unverifiable download was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if left := entries(t, dir); len(left) != 0 {
				t.Errorf("a failed download left %v behind", left)
			}
		})
	}
}

// The identity is the whole of what a keyless signature proves. Verifying
// against the wrong one, or forgetting the issuer, would accept a signature
// made by anybody with a GitHub account.
func TestTheSignatureIsCheckedAgainstThisRepositorysWorkflow(t *testing.T) {
	t.Parallel()

	github := &fakeGitHub{}
	source := github.start(t)
	binary, args := stubCosign(t, 0)
	source.CosignBinary = binary

	dir := t.TempDir()
	artifact, err := source.Fetch(context.Background(), release.Release{Tag: github.tag}, "amd64", dir)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !artifact.Signed {
		t.Error("Signed is false although cosign verified it")
	}

	invocation := args()
	for _, want := range []string{
		"verify-blob",
		"--certificate-identity-regexp",
		`https://github\.com/` + release.Repo + `/`,
		"--certificate-oidc-issuer",
		"https://token.actions.githubusercontent.com",
	} {
		if !strings.Contains(invocation, want) {
			t.Errorf("cosign was not given %q; it was given: %s", want, invocation)
		}
	}

	// The bundle is a means, not a result. Leaving it in /usr/local/bin would
	// be litter next to the binary it vouched for.
	left := entries(t, dir)
	if len(left) != 1 {
		t.Errorf("expected only the verified binary, found %v", left)
	}
}

// The policy has to hold on a redirect, not only on the first request: a
// release page that could send the download somewhere else is the whole reason
// the policy exists.
func TestADownloadRedirectedOffGitHubIsRefused(t *testing.T) {
	t.Parallel()

	github := &fakeGitHub{redirectAsset: "http://example.com/dokkup_linux_amd64"}
	source := github.start(t)

	dir := t.TempDir()
	_, err := source.Fetch(context.Background(), release.Release{Tag: github.tag}, "amd64", dir)
	if err == nil {
		t.Fatal("a download redirected off GitHub was followed")
	}
	if !strings.Contains(err.Error(), "example.com") {
		t.Errorf("the error does not say where it refused to go: %v", err)
	}
	if left := entries(t, dir); len(left) != 0 {
		t.Errorf("the refused download left %v behind", left)
	}
}

func TestAnInterruptedUpdateIsSweptUpByTheNextOne(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, name := range []string{
		release.TempPrefix + "123",
		release.TempPrefix + "456",
		".dokkup.bundle-789",
		"dokkup", // the installed binary, which must survive
		"other",
	} {
		if err := os.WriteFile(dir+"/"+name, []byte("x"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	release.SweepTemp(dir)

	left := entries(t, dir)
	if len(left) != 2 {
		t.Fatalf("left = %v, want the installed binary and the unrelated file", left)
	}
	for _, name := range left {
		if strings.HasPrefix(name, release.TempPrefix) {
			t.Errorf("%s survived the sweep", name)
		}
	}
}
