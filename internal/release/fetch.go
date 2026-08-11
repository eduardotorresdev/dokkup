package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// TempPrefix marks a partially-installed binary. Anything with this prefix is a
// leftover from a run that died, and is safe for the next run to sweep away.
const TempPrefix = ".dokkup.new-"

// bundlePrefix marks the signature bundle, which never outlives the check.
const bundlePrefix = ".dokkup.bundle-"

// maxAssetBytes bounds a download. The binary has a 25 MB budget
// (CONTRIBUTING.md); this leaves room to grow and still refuses to fill a disk
// with whatever an unexpected response turns out to be.
const maxAssetBytes = 200 << 20

// Artifact is a release binary that has been downloaded and verified.
type Artifact struct {
	// Path is the verified binary, in the directory Fetch was given. The caller
	// owns it: install it, or remove it.
	Path string

	// Name is the release asset it came from, e.g. "dokkup_linux_amd64".
	Name string

	// Signed reports whether the signature was checked. False means cosign was
	// not installed, so the binary is known to be the published bytes but
	// nothing has vouched for who published them.
	Signed bool
}

// AssetName is the release asset for an architecture, matching the names
// goreleaser publishes and install.sh downloads.
func AssetName(goarch string) (string, error) {
	switch goarch {
	case "amd64", "arm64":
		return "dokkup_linux_" + goarch, nil
	default:
		return "", fmt.Errorf("no dokkup release is published for %s/%s", runtime.GOOS, goarch)
	}
}

// Fetch downloads the release binary for goarch into dir and verifies it.
//
// This is install.sh:62-82 in Go, and it has to stay that way: the two are the
// only ways dokkup arrives on a host, and a difference between them would mean
// one of the paths trusts something the other rejects. Download the asset and
// SHA256SUMS, check the checksum, then check the signature when cosign is
// present.
//
// A failure at any point leaves nothing behind in dir and nothing at all
// touched outside it. There is no partial success: a binary that failed
// verification is removed rather than reported.
func (s *Source) Fetch(ctx context.Context, rel Release, goarch, dir string) (Artifact, error) {
	name, err := AssetName(goarch)
	if err != nil {
		return Artifact{}, err
	}
	if !tagPattern.MatchString(rel.Tag) {
		return Artifact{}, fmt.Errorf("%q is not a version like v0.1.0", rel.Tag)
	}
	base := s.baseURL() + "/download/" + rel.Tag

	binary, err := os.CreateTemp(dir, TempPrefix+"*")
	if err != nil {
		return Artifact{}, fmt.Errorf("preparing a place to download to: %w", err)
	}
	path := binary.Name()

	// Every path out of here from this point either returns the artifact or
	// removes it. Nothing half-verified is left where an installer could find
	// it later and mistake it for a finished download.
	fail := func(format string, a ...any) (Artifact, error) {
		_ = binary.Close()
		_ = os.Remove(path)
		return Artifact{}, fmt.Errorf(format, a...)
	}

	sum := sha256.New()
	if err := s.download(ctx, base+"/"+name, io.MultiWriter(binary, sum)); err != nil {
		return fail("downloading %s: %w", name, err)
	}
	if err := binary.Sync(); err != nil {
		return fail("flushing %s: %w", name, err)
	}
	if err := binary.Close(); err != nil {
		return fail("writing %s: %w", name, err)
	}

	var sums []byte
	if err := s.download(ctx, base+"/SHA256SUMS", writerTo(&sums)); err != nil {
		return fail("downloading SHA256SUMS: %w", err)
	}

	want, err := checksumFor(sums, name)
	if err != nil {
		return fail("%w", err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return fail("checksum mismatch for %s: SHA256SUMS says %s, the download is %s -- do not run this binary", name, want, got)
	}

	// The binary is about to replace one systemd executes, so it has to be
	// executable. It is still only readable where it sits, in a directory owned
	// by root.
	if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // an executable has to be executable
		return fail("making %s executable: %w", name, err)
	}

	signed, err := s.verifySignature(ctx, rel, name, path, dir)
	if err != nil {
		return fail("%w", err)
	}

	return Artifact{Path: path, Name: name, Signed: signed}, nil
}

// download writes one URL's body to w, refusing anything that is not a plain
// successful response.
//
// The transfer is bounded by progress rather than by a deadline. A total
// deadline cannot tell a 25 MB binary over a slow link from a server that has
// stopped sending, and it cannot be inherited from the caller either: the
// context an update passes here is the same one its rollback runs under, so a
// deadline that expired mid-download would take the rollback's own systemctl
// calls with it and report a broken host where there is a slow one. What is
// refused instead is silence -- see [stallTimeout].
func (s *Source) download(ctx context.Context, url string, w io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}

	body, stalled := watchProgress(ctx, cancel, resp.Body, s.stall())

	written, err := io.Copy(w, io.LimitReader(body, maxAssetBytes+1))
	if err != nil {
		if stalled() {
			return fmt.Errorf("%s: nothing arrived for %s, so the download was given up on", url, s.stall())
		}
		return err
	}
	if written > maxAssetBytes {
		return fmt.Errorf("%s: larger than %d bytes, which is not a dokkup release", url, maxAssetBytes)
	}
	return nil
}

// watchProgress cancels the request when the body goes quiet for limit, and
// reports afterwards whether that is what happened.
//
// The watchdog is what keeps `sudo dokkup update` from sitting at "Downloading"
// forever against a server that accepted the connection and then said nothing.
// Every read resets it, so a slow transfer that is still arriving is left alone.
func watchProgress(ctx context.Context, cancel context.CancelFunc, body io.Reader, limit time.Duration) (io.Reader, func() bool) {
	progress := make(chan struct{}, 1)
	var quiet atomic.Bool

	go func() {
		timer := time.NewTimer(limit)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-progress:
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(limit)
			case <-timer.C:
				quiet.Store(true)
				cancel()
				return
			}
		}
	}()

	reader := readerFunc(func(p []byte) (int, error) {
		n, err := body.Read(p)
		if n > 0 {
			select {
			case progress <- struct{}{}:
			default:
			}
		}
		return n, err
	})

	return reader, quiet.Load
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// writerTo collects a small response into memory.
func writerTo(dst *[]byte) io.Writer { return (*byteSink)(dst) }

type byteSink []byte

func (b *byteSink) Write(p []byte) (int, error) {
	*b = append(*b, p...)
	return len(p), nil
}

// SweepTemp removes what an interrupted update left in dir.
//
// A run killed between downloading and installing leaves a partial binary
// behind. Nothing reads those files, but they sit in /usr/local/bin
// accumulating, so the next update clears them before adding its own.
func SweepTemp(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, TempPrefix) || strings.HasPrefix(name, bundlePrefix) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}
