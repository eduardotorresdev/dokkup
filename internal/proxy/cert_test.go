package proxy_test

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/proxy"
)

// probeIP is the documentation range, so a certificate that escaped a test
// vouches for an address nobody can be reached on.
const probeIP = "192.0.2.7"

// selfSigned generates one throwaway certificate and fails the test rather than
// returning an error, so that every test below reads as what it is asserting.
func selfSigned(t *testing.T) (certPEM, keyPEM []byte, fingerprint string) {
	t.Helper()

	certPEM, keyPEM, fingerprint, err := proxy.SelfSigned(net.ParseIP(probeIP), time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("generating a certificate: %v", err)
	}
	return certPEM, keyPEM, fingerprint
}

// derOf parses the certificate back out of its PEM, which is the whole point of
// the test below: the fingerprint must be of these bytes and not of the text
// they arrived in.
func derOf(t *testing.T, certPEM []byte) []byte {
	t.Helper()

	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("the certificate is not PEM:\n%s", certPEM)
	}
	if block.Type != "CERTIFICATE" {
		t.Fatalf("PEM block = %q, want CERTIFICATE", block.Type)
	}
	return block.Bytes
}

// The number dokkup prints is the number an operator compares against what
// their browser shows, so it has to be the hash of the certificate. Hashing the
// PEM would produce something stable and plausible that matches nothing
// anywhere else, which is the worst kind of wrong: it looks like it works.
func TestTheFingerprintIsTheOneOfTheCertificateAndNotOfItsEncoding(t *testing.T) {
	t.Parallel()

	certPEM, _, fingerprint := selfSigned(t)

	sum := sha256.Sum256(derOf(t, certPEM))
	pairs := make([]string, 0, len(sum))
	for _, b := range sum {
		pairs = append(pairs, fmt.Sprintf("%02X", b))
	}
	if want := strings.Join(pairs, ":"); fingerprint != want {
		t.Errorf("fingerprint = %q, want %q", fingerprint, want)
	}

	if fingerprint == proxy.Fingerprint(certPEM) {
		t.Error("the fingerprint is of the PEM text rather than of the certificate")
	}

	// The form openssl and every browser print, because it is read by eye.
	if got := len(strings.Split(fingerprint, ":")); got != sha256.Size {
		t.Errorf("groups = %d, want %d: %q", got, sha256.Size, fingerprint)
	}
	if fingerprint != strings.ToUpper(fingerprint) {
		t.Errorf("the fingerprint is not upper case: %q", fingerprint)
	}
}

// A CommonName alone is ignored by every browser shipped this decade, so a
// certificate that named the address only in its subject would look right in
// `openssl x509 -text` and fail at the only moment that counts.
func TestTheCertificateNamesTheAddressWhereABrowserWillLookForIt(t *testing.T) {
	t.Parallel()

	certPEM, _, _ := selfSigned(t)

	cert, err := x509.ParseCertificate(derOf(t, certPEM))
	if err != nil {
		t.Fatalf("parsing the certificate: %v", err)
	}

	if len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(net.ParseIP(probeIP)) {
		t.Errorf("subject alternative names = %v, want [%s]", cert.IPAddresses, probeIP)
	}
	if err := cert.VerifyHostname(probeIP); err != nil {
		t.Errorf("the certificate does not vouch for %s: %v", probeIP, err)
	}

	// Chrome refuses a certificate without server authentication, and the
	// operator can only add this one to a trust store as its own root if it
	// says it is one.
	if !cert.IsCA {
		t.Error("the certificate cannot be trusted as its own root")
	}
	var serverAuth bool
	for _, usage := range cert.ExtKeyUsage {
		if usage == x509.ExtKeyUsageServerAuth {
			serverAuth = true
		}
	}
	if !serverAuth {
		t.Error("the certificate does not say it is for server authentication")
	}
}

func TestTheKeyThatComesBackOpensTheCertificateItCameWith(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM, _ := selfSigned(t)

	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("the certificate and the key are not a pair: %v", err)
	}
}

// A certificate generated seconds ago is rejected outright by a client whose
// clock is a few minutes fast, and all the operator would have to go on is "not
// yet valid" on a certificate dated today.
func TestTheCertificateIsAlreadyValidWhenAClockIsFast(t *testing.T) {
	t.Parallel()

	notAfter := time.Now().Add(24 * time.Hour)

	certPEM, _, _, err := proxy.SelfSigned(net.ParseIP(probeIP), notAfter)
	if err != nil {
		t.Fatalf("generating a certificate: %v", err)
	}

	cert, err := x509.ParseCertificate(derOf(t, certPEM))
	if err != nil {
		t.Fatalf("parsing the certificate: %v", err)
	}

	if !cert.NotBefore.Before(time.Now().Add(-time.Minute)) {
		t.Errorf("valid from %s, which leaves no room for a fast clock", cert.NotBefore)
	}
	// X.509 keeps whole seconds, so the round trip is not exact.
	if drift := cert.NotAfter.Sub(notAfter); drift > time.Second || drift < -time.Second {
		t.Errorf("valid until %s, want %s", cert.NotAfter, notAfter)
	}
}

// Two hosts, or one host installed twice, must not present certificates a trust
// store cannot tell apart.
func TestEachCertificateIsItsOwnAndNotACopyOfTheLast(t *testing.T) {
	t.Parallel()

	_, _, first := selfSigned(t)
	_, _, second := selfSigned(t)

	if first == second {
		t.Errorf("two certificates have the same fingerprint: %s", first)
	}
}

func TestACertificateForNoAddressIsRefusedRatherThanIssued(t *testing.T) {
	t.Parallel()

	if _, _, _, err := proxy.SelfSigned(nil, time.Now().Add(24*time.Hour)); err == nil {
		t.Fatal("a certificate that vouches for no address was issued")
	}
}

// The UDP dial asks the kernel for the source address of the default route.
// Reading the wrong end of that connection would return the probe address every
// time and look entirely plausible, which is why this asserts the answer is an
// address this host actually has.
func TestLocalIPReportsAnAddressThisHostHasRatherThanTheOneItAskedAbout(t *testing.T) {
	t.Parallel()

	ip, err := proxy.LocalIP()
	if err != nil {
		t.Skipf("this host has no route to take an address from: %v", err)
	}

	if ip.String() == "1.1.1.1" {
		t.Fatal("the address probed came back instead of the source address")
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("listing this host's addresses: %v", err)
	}
	for _, addr := range addrs {
		if network, ok := addr.(*net.IPNet); ok && network.IP.Equal(ip) {
			return
		}
	}
	t.Errorf("LocalIP = %s, which is on no interface of this host: %v", ip, addrs)
}
