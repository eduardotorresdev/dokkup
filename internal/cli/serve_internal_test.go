package cli

import (
	"bytes"
	"strings"
	"testing"
)

// serve is what the unit runs, so a flag combination it cannot act on has to be
// refused where it can still be read: on stderr, at start-up, rather than as a
// service that comes up healthy and quietly never gets a certificate.
func TestServeRefusesToManageACertificateWithNoDomainToGetOneFor(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr}

	err := runServe(env, []string{"--manage-certificate"})
	if err == nil {
		t.Fatal("serve accepted --manage-certificate with nothing to get a certificate for")
	}
	if !strings.Contains(err.Error(), "--domain") {
		t.Errorf("the refusal does not name the flag that is missing: %v", err)
	}
}
