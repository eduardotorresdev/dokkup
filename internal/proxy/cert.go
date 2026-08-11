package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// clockSlack is how long before now the certificate is already valid. A
// certificate generated seconds ago is rejected outright by a client whose
// clock is a few minutes fast, and the only thing the operator would have to go
// on is "not yet valid" on a certificate dated today.
const clockSlack = time.Hour

// serialBits is the width of the random serial number. A serial has to be
// unique among certificates from the same issuer, and this certificate issues
// itself, so 128 random bits is not a security property here -- it is what
// stops two hosts, or one host installed twice, presenting certificates a trust
// store cannot tell apart.
const serialBits = 128

// SelfSigned generates a certificate for reaching dokkup by IP address.
//
// The address goes in the SAN IPAddresses field. A CommonName alone is ignored
// by every browser shipped this decade, so a certificate that named the address
// only in its subject would look correct in `openssl x509 -text` and fail
// everywhere it actually matters. Measured with the address in IPAddresses:
// nginx 1.24 loaded it, `curl -k` got 200, and `curl --cacert` verified it.
//
// IsCA and KeyUsageCertSign are kept so that an operator can add this one
// certificate to a trust store as its own root rather than being asked to trust
// a leaf; ExtKeyUsageServerAuth is there because Chrome refuses a certificate
// without it. KeyEncipherment is inert for an ECDSA key and is in the set
// because this is the exact certificate that was measured loading and verifying
// -- `X509v3 Key Usage: critical Digital Signature, Key Encipherment,
// Certificate Sign`.
//
// The key is ECDSA P-256 and not RSA because it is generated while an operator
// waits on `dokkup install`: P-256 is instant where a 4096-bit RSA key is
// seconds to tens of seconds, and nothing that will ever speak to a certificate
// created today predates P-256.
//
// The fingerprint returned is [Fingerprint] of this certificate, which is what
// installation prints for the operator to compare against what their browser
// shows them.
func SelfSigned(ip net.IP, notAfter time.Time) (certPEM, keyPEM []byte, fingerprint string, err error) {
	if ip == nil {
		return nil, nil, "", errors.New("no address to make a certificate for")
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generating a key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, nil, "", fmt.Errorf("choosing a serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"dokkup"},
			CommonName:   ip.String(),
		},
		IPAddresses:           []net.IP{ip},
		NotBefore:             time.Now().Add(-clockSlack),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("signing the certificate: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("encoding the key: %w", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		Fingerprint(der),
		nil
}

// Fingerprint formats the SHA-256 of a certificate's DER as a browser shows it.
//
// The DER, never the PEM. The PEM is base64 of those same bytes with a header
// glued on, so hashing it yields a number that is stable, plausible and equal
// to nothing the operator can see anywhere else -- which is the worst kind of
// wrong, because it looks like it is working.
//
// Measured byte for byte identical, from all three sources, for the same
// certificate:
//
//	Go, sha256 of the DER  63:B9:DC:92:29:FB:F0:18:B5:A7:1D:52:32:A5:80:F0:...
//	openssl x509 -fingerprint -sha256          the same 32 pairs
//	openssl s_client against the live listener the same 32 pairs
//
// That is the whole point: the number dokkup prints is the number the operator
// compares in the browser.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)

	pairs := make([]string, 0, len(sum))
	for _, b := range sum {
		pairs = append(pairs, fmt.Sprintf("%02X", b))
	}
	return strings.Join(pairs, ":")
}

// routeProbe is an address that is never contacted. It only has to be routable
// for the kernel to pick a source address for it, and it is the address `ip
// route get` is conventionally asked about, so an operator can check the answer
// by hand with a command they already know.
const routeProbe = "1.1.1.1:80"

// LocalIP reports the address this host would use to reach the internet.
//
// A UDP "connection" sends no packet; it asks the kernel for the source address
// of the default route, which is the same answer `ip route get 1.1.1.1` gives
// -- measured, both said 192.168.65.7 on the same host -- with no subprocess
// and no output to parse. net.InterfaceAddrs is the wrong tool here: every
// Dokku host has a docker0, 172.17.0.1 on the host this was measured on, so
// discarding loopback still leaves a choice between two private addresses and
// nothing to choose on.
//
// What it cannot know is whether the world reaches the host on that address.
// Behind NAT -- which is every AWS, GCP and Azure instance -- it does not: the
// host sees a private address while the public record points at the NAT, and
// nothing running on the host can tell the two apart. So the caller prints the
// address this picked, says so when it is private, and takes an override.
// DialUDP rather than the shorter Dial: the address is a literal, so nothing is
// resolved, and there is no context to thread through a call that performs no
// I/O in the first place.
func LocalIP() (net.IP, error) {
	probe, err := net.ResolveUDPAddr("udp", routeProbe)
	if err != nil {
		return nil, fmt.Errorf("reading the probe address %s: %w", routeProbe, err)
	}

	conn, err := net.DialUDP("udp", nil, probe)
	if err != nil {
		return nil, fmt.Errorf("asking this host which address reaches the internet: %w", err)
	}
	defer func() { _ = conn.Close() }()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil || addr.IP.IsUnspecified() {
		return nil, errors.New("this host has no route to take an address from")
	}
	return addr.IP, nil
}
