package acme_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/acme"
)

// fakeCA is an ACME server: as much of RFC 8555 as one order needs, and no more.
//
// It exists because an ACME client is a protocol state machine, and the only
// way to find out whether a state machine works is to run it. Pebble proves the
// same code against a real implementation, but that wants a container and a
// network; this runs under `go test` on any machine, so the wiring is covered on
// every commit rather than on the days somebody remembers to bring the dev
// environment up.
//
// Signatures are not verified. What is under test is dokkup's half of the
// conversation -- that it registers, orders, presents the token at the path a
// certificate authority fetches, and writes what comes back -- and a test that
// re-implemented JWS verification would be testing golang.org/x/crypto instead.
type fakeCA struct {
	// validateAt is the dokkup this authority fetches the challenge from,
	// standing in for the domain that would resolve to it in production.
	validateAt string

	// refuse fails validation however good the answer is, which is what a
	// firewalled port 80 looks like from in here.
	refuse bool

	// slowFinalize answers the finalize request the way Pebble 2.10.1 does:
	// status "processing", a Retry-After, and no Location header for the client
	// to read the order's own URL back out of.
	slowFinalize bool

	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey

	// mu guards everything below it. The handlers answer on the test server's
	// own goroutines while the test reads what they recorded.
	mu        sync.Mutex
	nonce     int
	orders    int
	finalized bool
	domain    string
	token     string
	answered  string
	fetched   []string
	authz     string
	order     string

	// chain is what /cert hands back: the leaf this authority signed, then
	// itself, which is the shape a real one returns.
	chain []byte
}

// start brings the authority up and returns its directory URL.
func (f *fakeCA) start(t *testing.T) string {
	t.Helper()

	f.makeCA(t)
	f.token = "a-token-the-authority-chose"
	f.authz, f.order = "pending", "pending"

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	base := server.URL

	// Every answer carries a nonce, because the client spends one per request
	// and fetching a fresh one for each would be four round trips per order for
	// nothing.
	nonce := func(w http.ResponseWriter) {
		f.mu.Lock()
		f.nonce++
		n := f.nonce
		f.mu.Unlock()
		w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce-%d", n))
	}

	reply := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}

	mux.HandleFunc("/directory", func(w http.ResponseWriter, _ *http.Request) {
		nonce(w)
		reply(w, http.StatusOK, map[string]any{
			"newNonce":   base + "/new-nonce",
			"newAccount": base + "/new-account",
			"newOrder":   base + "/new-order",
			"revokeCert": base + "/revoke",
			"keyChange":  base + "/key-change",
		})
	})

	mux.HandleFunc("/new-nonce", func(w http.ResponseWriter, _ *http.Request) {
		nonce(w)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/new-account", func(w http.ResponseWriter, _ *http.Request) {
		nonce(w)
		w.Header().Set("Location", base+"/account/1")
		reply(w, http.StatusCreated, map[string]any{"status": "valid"})
	})

	mux.HandleFunc("/new-order", func(w http.ResponseWriter, r *http.Request) {
		nonce(w)

		var body struct {
			Identifiers []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"identifiers"`
		}
		if err := json.Unmarshal(payloadOf(t, r), &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		f.mu.Lock()
		f.orders++
		f.finalized = false
		f.authz, f.order = "pending", "pending"
		if len(body.Identifiers) > 0 {
			f.domain = body.Identifiers[0].Value
		}
		f.mu.Unlock()

		w.Header().Set("Location", base+"/order")
		reply(w, http.StatusCreated, f.orderBody(base))
	})

	// No Location header, here or on finalize, which is what a real authority
	// is entitled to do: RFC 8555 conveys the order's URL when the order is
	// created and nowhere else.
	mux.HandleFunc("/order", func(w http.ResponseWriter, _ *http.Request) {
		nonce(w)
		reply(w, http.StatusOK, f.orderBody(base))
	})

	mux.HandleFunc("/authz", func(w http.ResponseWriter, _ *http.Request) {
		nonce(w)
		reply(w, http.StatusOK, f.authzBody(base))
	})

	mux.HandleFunc("/challenge", func(w http.ResponseWriter, _ *http.Request) {
		nonce(w)
		f.validate()
		reply(w, http.StatusOK, f.challengeBody(base))
	})

	mux.HandleFunc("/finalize", func(w http.ResponseWriter, r *http.Request) {
		nonce(w)

		var body struct {
			CSR string `json:"csr"`
		}
		if err := json.Unmarshal(payloadOf(t, r), &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		der, err := base64.RawURLEncoding.DecodeString(body.CSR)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := f.issue(der); err != nil {
			t.Errorf("the fake authority could not sign the request: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		f.mu.Lock()
		f.finalized = true
		slow := f.slowFinalize
		f.mu.Unlock()

		if slow {
			w.Header().Set("Retry-After", "0")
			body := f.orderBody(base)
			body["status"] = "processing"
			delete(body, "certificate")
			reply(w, http.StatusOK, body)
			return
		}
		reply(w, http.StatusOK, f.orderBody(base))
	})

	mux.HandleFunc("/cert", func(w http.ResponseWriter, _ *http.Request) {
		nonce(w)
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		f.mu.Lock()
		defer f.mu.Unlock()
		_, _ = w.Write(f.chain)
	})

	return base + "/directory"
}

func (f *fakeCA) orderBody(base string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	body := map[string]any{
		"status":         f.order,
		"authorizations": []string{base + "/authz"},
		"finalize":       base + "/finalize",
	}
	if f.finalized {
		body["status"] = "valid"
		body["certificate"] = base + "/cert"
	}
	return body
}

func (f *fakeCA) authzBody(base string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]any{
		"status":     f.authz,
		"identifier": map[string]string{"type": "dns", "value": f.domain},
		"challenges": []map[string]any{{
			"type":   "http-01",
			"url":    base + "/challenge",
			"token":  f.token,
			"status": f.authz,
		}},
	}
}

func (f *fakeCA) challengeBody(base string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]any{
		"type":   "http-01",
		"url":    base + "/challenge",
		"token":  f.token,
		"status": f.authz,
	}
}

// validate is the part that matters: the authority goes and fetches the token
// from the host claiming the name, exactly as a real one does, over the vhost's
// own location block if the test put one in front.
func (f *fakeCA) validate() {
	f.mu.Lock()
	path := acme.ChallengePath + f.token
	f.fetched = append(f.fetched, path)
	where := f.validateAt + path
	refuse := f.refuse
	token := f.token
	f.mu.Unlock()

	var answered string
	good := false
	if resp, err := http.Get(where); err == nil { //nolint:gosec,noctx // a loopback test server
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		answered = string(body)
		// The key authorization is the token, a dot, and the account key's
		// thumbprint. Checking the prefix proves dokkup answered for the right
		// token; checking the thumbprint would be re-implementing JWS.
		good = resp.StatusCode == http.StatusOK && strings.HasPrefix(answered, token+".")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = answered
	if good && !refuse {
		f.authz, f.order = "valid", "ready"
		return
	}
	f.authz, f.order = "invalid", "invalid"
}

func (f *fakeCA) issue(csrDER []byte) error {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: csr.Subject.CommonName},
		DNSNames:     csr.DNSNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, f.caCert, csr.PublicKey, f.caKey)
	if err != nil {
		return err
	}

	var out strings.Builder
	for _, block := range [][]byte{der, f.caCert.Raw} {
		if err := pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: block}); err != nil {
			return err
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.chain = []byte(out.String())
	return nil
}

func (f *fakeCA) makeCA(t *testing.T) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating the fake authority's key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dokkup test authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatalf("generating the fake authority's certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("reading the fake authority's certificate back: %v", err)
	}

	f.caKey, f.caCert = key, cert
}

func (f *fakeCA) ordered() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orders
}

func (f *fakeCA) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.fetched)
}

func (f *fakeCA) gotBack() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.answered
}

// payloadOf reads what the client signed. The signature is not checked, for the
// reason on [fakeCA].
func payloadOf(t *testing.T, r *http.Request) []byte {
	t.Helper()

	var jws struct {
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&jws); err != nil {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(jws.Payload)
	if err != nil {
		return nil
	}
	return raw
}
