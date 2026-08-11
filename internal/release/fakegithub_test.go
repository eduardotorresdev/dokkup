package release_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/release"
	"github.com/eduardotorresdev/dokkup/internal/stubprog"
)

// fakeGitHub serves what a real release looks like from the outside: the API
// answer, the binary, SHA256SUMS and the signature bundle.
//
// The tests drive the Source through its ordinary HTTP client rather than a
// stub one, so the transport policy, the redirect handling and the download all
// run for real against a loopback address.
type fakeGitHub struct {
	tag    string
	binary []byte
	bundle []byte

	// sums replaces the computed SHA256SUMS when set, for corrupting it.
	sums string

	// missing 404s the paths named in it, keyed by asset file name.
	missing map[string]bool

	// redirectAsset, when set, is where the binary download is sent instead.
	redirectAsset string

	// draft marks the release as one GitHub has not published.
	draft bool

	// requested records every path asked for, in order. Guarded because the
	// server answers on its own goroutines.
	mu        sync.Mutex
	requested []string
}

func (f *fakeGitHub) record(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requested = append(f.requested, path)
}

func (f *fakeGitHub) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requested)
}

func (f *fakeGitHub) start(t *testing.T) *release.Source {
	t.Helper()

	if f.tag == "" {
		f.tag = "v0.2.0"
	}
	if f.binary == nil {
		f.binary = []byte("#!/bin/sh\necho the new dokkup\n")
	}
	if f.bundle == nil {
		f.bundle = []byte("{\"a\": \"signature bundle\"}")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r.URL.Path)

		switch {
		case r.URL.Path == "/api/releases/latest",
			r.URL.Path == "/api/releases/tags/"+f.tag:
			if f.missing["release"] {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": f.tag,
				"html_url": "https://github.com/" + release.Repo + "/releases/tag/" + f.tag,
				"draft":    f.draft,
			})

		case strings.HasPrefix(r.URL.Path, "/api/releases/tags/"):
			http.NotFound(w, r)

		default:
			f.serveAsset(w, r)
		}
	}))
	t.Cleanup(server.Close)

	return &release.Source{
		BaseURL: server.URL + "/releases",
		APIURL:  server.URL + "/api",
	}
}

func (f *fakeGitHub) serveAsset(w http.ResponseWriter, r *http.Request) {
	prefix := "/releases/download/" + f.tag + "/"
	name, found := strings.CutPrefix(r.URL.Path, prefix)
	if !found || f.missing[name] {
		http.NotFound(w, r)
		return
	}

	switch {
	case name == "SHA256SUMS":
		_, _ = w.Write([]byte(f.checksums()))

	case strings.HasSuffix(name, ".cosign.bundle"):
		_, _ = w.Write(f.bundle)

	default:
		if f.redirectAsset != "" {
			http.Redirect(w, r, f.redirectAsset, http.StatusFound)
			return
		}
		_, _ = w.Write(f.binary)
	}
}

func (f *fakeGitHub) checksums() string {
	if f.sums != "" {
		return f.sums
	}
	sum := sha256.Sum256(f.binary)
	return fmt.Sprintf("%s  dokkup_linux_amd64\n%s  dokkup_linux_arm64\n",
		hex.EncodeToString(sum[:]), hex.EncodeToString(sum[:]))
}

// stubCosign puts a stand-in for cosign in its own directory and returns its
// path, along with a reader for the arguments it was handed. Real signature
// verification needs a real signature and the public transparency log; what
// belongs in a unit test is that cosign is invoked with the identity dokkup
// means to require, and that its verdict is obeyed.
func stubCosign(t *testing.T, exitCode int) (binary string, args func() string) {
	t.Helper()

	dir := t.TempDir()
	binary = filepath.Join(dir, "cosign")
	log := filepath.Join(dir, "args")

	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" > '%s'\n"+
		"echo 'stub cosign speaking' >&2\nexit %d\n", log, exitCode)
	if err := stubprog.Write(binary, script); err != nil {
		t.Fatalf("writing the cosign stub: %v", err)
	}

	return binary, func() string {
		content, err := os.ReadFile(log)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(content))
	}
}

// entries lists what is left in a directory, so a test can assert that a failed
// download left nothing behind.
func entries(t *testing.T, dir string) []string {
	t.Helper()

	found, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	names := make([]string, 0, len(found))
	for _, entry := range found {
		names = append(names, entry.Name())
	}
	return names
}
