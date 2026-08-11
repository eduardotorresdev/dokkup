package release

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CosignBinary is the signature verifier looked for on PATH.
const CosignBinary = "cosign"

// oidcIssuer is the identity provider a dokkup signature is bound to. Keyless
// signing means there is no key to compare against: what is checked is that the
// signature was made by this repository's workflow, with GitHub's OIDC token as
// the proof. Both halves must match install.sh, or the two install paths would
// accept different signatures.
const oidcIssuer = "https://token.actions.githubusercontent.com"

// identityRegexp is the certificate identity a signature must carry.
var identityRegexp = `https://github\.com/` + Repo + `/`

// cosignTimeout bounds verification. cosign consults the public transparency
// log, so this is a network operation and can hang rather than fail.
const cosignTimeout = 2 * time.Minute

// checksumFor returns the expected checksum of name from a SHA256SUMS body.
//
// An entry that is not there is an error, deliberately. `sha256sum --check
// --ignore-missing` skips names it cannot find, which is the right behaviour
// when checking a directory against a list and the wrong one here: an asset
// missing from SHA256SUMS is an unverified download, not a skipped one.
func checksumFor(sums []byte, name string) (string, error) {
	for line := range strings.SplitSeq(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// sha256sum writes "*name" for a file it read in binary mode.
		if strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != 64 {
			return "", fmt.Errorf("SHA256SUMS gives %s a checksum that is not a sha256", name)
		}
		return sum, nil
	}
	return "", fmt.Errorf("SHA256SUMS does not list %s -- nothing vouches for this download", name)
}

// verifySignature checks the release signature, and reports whether it could.
//
// Absent cosign the checksum still stands, so the download is known to be the
// published bytes; what is missing is any evidence of who published them. That
// is the same trade install.sh makes, and the caller says so out loud rather
// than letting it pass unmentioned.
func (s *Source) verifySignature(ctx context.Context, rel Release, name, path, dir string) (bool, error) {
	binary := s.CosignBinary
	if binary == "" {
		found, err := exec.LookPath(CosignBinary)
		if err != nil {
			return false, nil
		}
		binary = found
	}

	bundle, err := os.CreateTemp(dir, bundlePrefix+"*")
	if err != nil {
		return false, fmt.Errorf("preparing to check the signature: %w", err)
	}
	bundlePath := bundle.Name()
	defer func() { _ = os.Remove(bundlePath) }()

	url := s.baseURL() + "/download/" + rel.Tag + "/" + name + ".cosign.bundle"
	if err := s.download(ctx, url, bundle); err != nil {
		_ = bundle.Close()
		return false, fmt.Errorf("downloading the signature for %s: %w", name, err)
	}
	if err := bundle.Close(); err != nil {
		return false, fmt.Errorf("writing the signature for %s: %w", name, err)
	}

	ctx, cancel := context.WithTimeout(ctx, cosignTimeout)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary,
		"verify-blob", path,
		"--bundle", bundlePath,
		"--certificate-identity-regexp", identityRegexp,
		"--certificate-oidc-issuer", oidcIssuer,
	)
	cmd.Stderr = &stderr
	cmd.Stdin = nil

	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("signature verification failed for %s: %w: %s -- do not run this binary",
			name, err, strings.TrimSpace(stderr.String()))
	}
	return true, nil
}
