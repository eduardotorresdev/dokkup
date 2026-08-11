package acme_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/acme"
	"github.com/eduardotorresdev/dokkup/internal/proxy"
)

const testDomain = "dokkup.example"

// harness is one manager, one authority, and the dokkup the authority fetches
// the challenge from, wired together the way the service wires them.
type harness struct {
	manager *acme.Manager
	ca      *fakeCA
	dir     string

	// reloads counts what nginx was asked to do, and reloadErr makes it fail.
	reloads   int
	reloadErr error
}

func newHarness(t *testing.T, ca *fakeCA) *harness {
	t.Helper()

	h := &harness{ca: ca, dir: t.TempDir()}

	h.manager = &acme.Manager{
		Domain: testDomain,
		Email:  "operator@dokkup.example",
		Dir:    h.dir,
		Reload: func(context.Context) error {
			h.reloads++
			return h.reloadErr
		},
	}

	// The same mux serve builds: the challenge ahead of everything else, and
	// dokkup itself underneath it. Serving it through a mux rather than calling
	// the handler directly is the point -- a handler mounted at the wrong prefix
	// would pass a direct call and fail every validation.
	mux := http.NewServeMux()
	mux.Handle(acme.ChallengePath, h.manager.Handler())
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("dokkup itself"))
	})

	dokkup := httptest.NewServer(mux)
	t.Cleanup(dokkup.Close)

	ca.validateAt = dokkup.URL
	h.manager.DirectoryURL = ca.start(t)

	return h
}

func (h *harness) leaf(t *testing.T) *x509.Certificate {
	t.Helper()

	certPEM, err := os.ReadFile(filepath.Join(h.dir, acme.CertFile))
	if err != nil {
		t.Fatalf("reading the certificate that was written: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("what was written to %s is not PEM", acme.CertFile)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing the certificate that was written: %v", err)
	}
	return leaf
}

func TestObtainingACertificateWritesTheChainAndItsKeyAndAsksNginxToReadThem(t *testing.T) {
	t.Parallel()

	ca := &fakeCA{}
	h := newHarness(t, ca)

	issued, err := h.manager.Ensure(t.Context())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !issued {
		t.Fatal("Ensure reported that it wrote nothing, on a host with no certificate at all")
	}

	leaf := h.leaf(t)
	if err := leaf.VerifyHostname(testDomain); err != nil {
		t.Errorf("the certificate does not cover %s: %v", testDomain, err)
	}
	if err := leaf.CheckSignatureFrom(ca.caCert); err != nil {
		t.Errorf("the certificate was not signed by the authority that issued it: %v", err)
	}

	// Both files, and the pair, because nginx loads them together and a
	// certificate written beside somebody else's key is a vhost that will not
	// start.
	certPEM, err := os.ReadFile(filepath.Join(h.dir, acme.CertFile))
	if err != nil {
		t.Fatalf("reading the certificate: %v", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(h.dir, acme.KeyFile))
	if err != nil {
		t.Fatalf("reading the private key: %v", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("the certificate and key that were written do not go together: %v", err)
	}
	if len(pair.Certificate) != 2 {
		t.Errorf("the chain has %d certificates, want the leaf and the issuer; nginx serves what "+
			"is in this file and a client that has to fetch the issuer itself often will not",
			len(pair.Certificate))
	}

	if h.reloads != 1 {
		t.Errorf("nginx was asked to reload %d times, want once: a certificate on disk that nginx "+
			"has not read is still the old certificate being served", h.reloads)
	}
}

func TestTheAuthorityFetchesTheTokenFromDokkupItselfAndGetsTheAnswerToItsOwnChallenge(t *testing.T) {
	t.Parallel()

	ca := &fakeCA{}
	h := newHarness(t, ca)

	if _, err := h.manager.Ensure(t.Context()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	asked := ca.asked()
	if len(asked) != 1 {
		t.Fatalf("the authority made %d requests to dokkup, want one: %v", len(asked), asked)
	}
	if want := acme.ChallengePath + ca.token; asked[0] != want {
		t.Errorf("the authority asked for %s, want %s", asked[0], want)
	}
	if got := ca.gotBack(); !strings.HasPrefix(got, ca.token+".") {
		t.Errorf("dokkup answered %q, want the token, a dot and the account key's thumbprint", got)
	}
}

// Finalizing an order may be asynchronous, and the certificate is then fetched
// from the order rather than from the finalize response.
//
// golang.org/x/crypto/acme polls for it at the URL it reads out of the finalize
// response's Location header, which RFC 8555 does not require an authority to
// send. Measured against Pebble 2.10.1, which sends Retry-After and no
// Location: the certificate was issued, the client had nowhere to poll, and
// issuance failed with `Post "": unsupported protocol scheme ""`. Let's Encrypt
// does send it, so nothing in production exercises this -- which is why it is
// asserted here rather than left to be discovered by every renewal failing at
// once on the day that changes.
func TestACertificateIssuedAfterTheFinalizeAnswerIsStillCollected(t *testing.T) {
	t.Parallel()

	ca := &fakeCA{slowFinalize: true}
	h := newHarness(t, ca)

	issued, err := h.manager.Ensure(t.Context())
	if err != nil {
		t.Fatalf("Ensure gave up on a certificate the authority had already issued: %v", err)
	}
	if !issued {
		t.Fatal("Ensure reported that it wrote nothing")
	}
	if err := h.leaf(t).CheckSignatureFrom(ca.caCert); err != nil {
		t.Errorf("what was written was not the certificate the authority issued: %v", err)
	}
}

func TestTheChallengeHandlerAnswersNothingItWasNotAskedToPresent(t *testing.T) {
	t.Parallel()

	ca := &fakeCA{}
	h := newHarness(t, ca)

	// Before any order, and after one has finished, the same path is nothing:
	// the token is forgotten as soon as it has been checked, so a running dokkup
	// cannot be read for one.
	for _, when := range []string{"before issuance", "after issuance"} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
			acme.ChallengePath+ca.token, nil)
		recorder := httptest.NewRecorder()
		h.manager.Handler().ServeHTTP(recorder, request)

		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s: asking for the token got %d, want 404", when, recorder.Code)
		}

		if when == "before issuance" {
			if _, err := h.manager.Ensure(t.Context()); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
		}
	}
}

func TestACertificateWithPlentyOfLifeLeftIsLeftAlone(t *testing.T) {
	t.Parallel()

	ca := &fakeCA{}
	h := newHarness(t, ca)

	if _, err := h.manager.Ensure(t.Context()); err != nil {
		t.Fatalf("the first Ensure: %v", err)
	}
	first := h.leaf(t)

	issued, err := h.manager.Ensure(t.Context())
	if err != nil {
		t.Fatalf("the second Ensure: %v", err)
	}
	if issued {
		t.Error("Ensure got a second certificate for a host that already had a good one; this " +
			"runs every twelve hours and would spend the account's issuance allowance on nothing")
	}
	if ca.ordered() != 1 {
		t.Errorf("the authority saw %d orders, want one", ca.ordered())
	}
	if h.reloads != 1 {
		t.Errorf("nginx was reloaded %d times, want once", h.reloads)
	}
	if !h.leaf(t).Equal(first) {
		t.Error("the certificate on disk was replaced")
	}
}

func TestACertificateInsideItsRenewalWindowIsReplacedBeforeItExpires(t *testing.T) {
	t.Parallel()

	ca := &fakeCA{}
	h := newHarness(t, ca)

	if _, err := h.manager.Ensure(t.Context()); err != nil {
		t.Fatalf("the first Ensure: %v", err)
	}
	first := h.leaf(t)

	// Seventy days on: the ninety-day certificate has twenty days left, which is
	// inside the thirty-day window, and still ten days of runway for the
	// failures that need a person.
	h.manager.Now = func() time.Time { return time.Now().Add(70 * 24 * time.Hour) }

	issued, err := h.manager.Ensure(t.Context())
	if err != nil {
		t.Fatalf("the renewal: %v", err)
	}
	if !issued {
		t.Fatal("a certificate with twenty days left was not renewed")
	}
	if ca.ordered() != 2 {
		t.Errorf("the authority saw %d orders, want two", ca.ordered())
	}
	if h.leaf(t).Equal(first) {
		t.Error("the certificate on disk is still the old one")
	}
}

func TestTheSelfSignedCertificateInstallationLeavesBehindIsAlwaysReplaced(t *testing.T) {
	t.Parallel()

	ca := &fakeCA{}
	h := newHarness(t, ca)

	// Written by the installer through the very function the installer calls, so
	// that the two cannot come to disagree about what a placeholder looks like.
	// It is valid for ten years, so nothing about its expiry would prompt this.
	certPEM, keyPEM, err := proxy.SelfSignedForDomain(testDomain, time.Now().Add(10*365*24*time.Hour))
	if err != nil {
		t.Fatalf("making the placeholder: %v", err)
	}
	write(t, filepath.Join(h.dir, acme.CertFile), certPEM)
	write(t, filepath.Join(h.dir, acme.KeyFile), keyPEM)

	issued, err := h.manager.Ensure(t.Context())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !issued {
		t.Fatal("the self-signed placeholder was left in place, so every browser would go on " +
			"warning about a certificate nobody issued")
	}
	if err := h.leaf(t).CheckSignatureFrom(ca.caCert); err != nil {
		t.Errorf("what is on disk was not signed by the authority: %v", err)
	}
}

func TestAValidationTheAuthorityCannotCompleteSaysWhatItCouldNotReach(t *testing.T) {
	t.Parallel()

	ca := &fakeCA{refuse: true}
	h := newHarness(t, ca)

	issued, err := h.manager.Ensure(t.Context())
	if err == nil {
		t.Fatal("Ensure reported success against an authority that refused every validation")
	}
	if issued {
		t.Error("Ensure reported that it wrote a certificate it never got")
	}

	// The message is the whole value of this path. An operator reading a journal
	// has to be able to tell "the name does not reach this host on port 80" from
	// "dokkup is broken", and only the first is worth their afternoon.
	if !strings.Contains(err.Error(), acme.ChallengePath) {
		t.Errorf("the error does not name the path that was not reachable: %v", err)
	}

	if _, err := os.Stat(filepath.Join(h.dir, acme.CertFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed issuance left a certificate behind: %v", err)
	}
	if h.reloads != 0 {
		t.Errorf("nginx was reloaded %d times after a failed issuance", h.reloads)
	}
}

func TestNginxNotReloadingIsReportedWithoutThrowingAwayTheCertificate(t *testing.T) {
	t.Parallel()

	ca := &fakeCA{}
	h := newHarness(t, ca)
	h.reloadErr = errors.New("sudo: a password is required")

	issued, err := h.manager.Ensure(t.Context())
	if err == nil {
		t.Fatal("Ensure said nothing about nginx refusing to reload, so the old certificate would " +
			"go on being served with nobody told")
	}
	if !issued {
		t.Error("Ensure reported that it wrote nothing, when the certificate is on disk; a caller " +
			"believing that would order another one every twelve hours")
	}
	if err := h.leaf(t).CheckSignatureFrom(ca.caCert); err != nil {
		t.Errorf("the certificate was not kept: %v", err)
	}
}

func TestTheAccountKeyIsKeptSoRenewalIsTheSameSubscriberAsking(t *testing.T) {
	t.Parallel()

	ca := &fakeCA{}
	h := newHarness(t, ca)

	if _, err := h.manager.Ensure(t.Context()); err != nil {
		t.Fatalf("the first Ensure: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(h.dir, acme.AccountKeyFile))
	if err != nil {
		t.Fatalf("reading the account key: %v", err)
	}

	h.manager.Now = func() time.Time { return time.Now().Add(70 * 24 * time.Hour) }
	if _, err := h.manager.Ensure(t.Context()); err != nil {
		t.Fatalf("the renewal: %v", err)
	}

	second, err := os.ReadFile(filepath.Join(h.dir, acme.AccountKeyFile))
	if err != nil {
		t.Fatalf("reading the account key again: %v", err)
	}
	if string(first) != string(second) {
		t.Error("the account key was regenerated; to a certificate authority that is a new " +
			"subscriber every renewal, with a new allowance and no history")
	}
}

func TestTheKeysAreWrittenWhereNothingElseOnTheHostCanReadThem(t *testing.T) {
	t.Parallel()

	ca := &fakeCA{}
	h := newHarness(t, ca)

	if _, err := h.manager.Ensure(t.Context()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	for name, want := range map[string]os.FileMode{
		acme.KeyFile:        0o600,
		acme.AccountKeyFile: 0o600,
		// Public by construction: every client that connects is handed it, and
		// nginx reads it as root in any case.
		acme.CertFile: 0o644,
	} {
		info, err := os.Stat(filepath.Join(h.dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s is %#o, want %#o", name, got, want)
		}
	}
}

func TestRunGetsTheCertificateStraightAwayRatherThanWaitingOutTheFirstInterval(t *testing.T) {
	t.Parallel()

	ca := &fakeCA{}
	h := newHarness(t, ca)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		h.manager.Run(ctx)
		close(done)
	}()

	// The installer starts the service precisely so that this happens, and then
	// stands there watching for the file.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(h.dir, acme.CertFile)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run had not obtained a certificate ten seconds in; an operator watching an " +
				"installation would have been told to come back in twelve hours")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return when its context was cancelled, so the service would not stop")
	}
}

func TestEnsureRefusesWithoutADomainRatherThanOrderingACertificateForNothing(t *testing.T) {
	t.Parallel()

	manager := &acme.Manager{Dir: t.TempDir()}

	if _, err := manager.Ensure(t.Context()); err == nil {
		t.Fatal("Ensure accepted a manager with no domain")
	}
}

func TestExpiryReportsWhatIsOnDisk(t *testing.T) {
	t.Parallel()

	ca := &fakeCA{}
	h := newHarness(t, ca)

	if _, err := h.manager.Expiry(); err == nil {
		t.Error("Expiry answered for a host that has no certificate")
	}

	if _, err := h.manager.Ensure(t.Context()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	got, err := h.manager.Expiry()
	if err != nil {
		t.Fatalf("Expiry: %v", err)
	}
	if want := h.leaf(t).NotAfter; !got.Equal(want) {
		t.Errorf("Expiry says %s, the certificate says %s", got, want)
	}
}

func write(t *testing.T, path string, content []byte) {
	t.Helper()

	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
