// Package acme obtains and renews the certificate dokkup serves at its own
// domain.
//
// It exists because Dokku cannot do this for dokkup. Dokku's Let's Encrypt
// plugin issues certificates for Apps, and dokkup is not an App and cannot be
// one (ADR-0001): measured against dokku-letsencrypt 0.25.1, every entry point
// calls verify_app_name and exits 20 for an argument that is not an App. The
// answer is not to become an App but to speak ACME directly, which costs one
// dependency and 12 KB of binary -- measured, 7,770,274 bytes to 7,782,562 --
// because dokkup already links crypto/tls, crypto/x509, crypto/ecdsa and
// net/http, so golang.org/x/crypto/acme adds little but its own logic.
//
// The challenge is served from memory rather than from a webroot. nginx's
// workers run as www-data on a Dokku host -- measured -- and dokkup's data
// directory is 0750 dokkup:dokkup, so a webroot would mean either loosening
// that mode or putting a second directory somewhere else on the host. Serving
// it from the running process costs one location block that proxies to the
// address dokkup already listens on, and leaves the token where the state
// machine that made it already is.
//
// See docs/adr/0006-publishing-tls-and-ip-mode.md.
package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

// ChallengePath is where a certificate authority looks for the HTTP-01 token.
//
// RFC 8555 §8.3 fixes it, so this is not a choice. It is exported because the
// nginx server block has to route exactly this prefix to dokkup, and a vhost
// that disagreed with this handler by one character would fail every
// validation while looking correct.
const ChallengePath = "/.well-known/acme-challenge/"

// File names under [Manager.Dir]. The certificate and its key are the two paths
// the vhost already names, so renewal replaces file contents and never rewrites
// the server block -- rewriting a vhost is the one operation that can leave a
// host unable to reach dokkup.
const (
	AccountKeyFile = "account.key"
	CertFile       = "cert.pem"
	KeyFile        = "key.pem"
)

const (
	// keyMode is what a private key is readable at. The account key is as
	// sensitive as the certificate key: whoever holds it can revoke this
	// host's certificates.
	keyMode = 0o600

	// certMode is what the certificate chain is readable at. It is public by
	// construction -- every client that connects is handed it.
	certMode = 0o644
)

// renewBefore is how much life a certificate has left when renewal starts.
//
// Let's Encrypt issues for 90 days and starts warning at 20; 30 leaves ten days
// of failed attempts before anyone is inconvenienced, which matters because the
// failures that need a person -- DNS moved, port 80 closed -- are exactly the
// ones no retry fixes.
const renewBefore = 30 * 24 * time.Hour

// How often the loop looks, and how soon it tries again after a failure.
//
// The retry interval is a rate limit, not a guess. Let's Encrypt allows five
// failed validations per account, per hostname, per hour; at fifteen minutes
// this makes four, which stays under it even if every attempt fails. Anything
// faster spends the allowance and then cannot retry when the cause is fixed.
const (
	checkInterval = 12 * time.Hour
	retryInterval = 15 * time.Minute
)

// Manager keeps one domain's certificate current.
//
// Everything it reaches the outside world through is a field, so a test can
// watch it issue against an ACME server of its own without a domain, a
// certificate authority or a network.
type Manager struct {
	// Domain is the name the certificate is for.
	Domain string

	// Email is the account contact. A certificate authority uses it to warn
	// about expiry, which is the last line of defence when renewal has been
	// failing unnoticed. Empty registers without one.
	Email string

	// DirectoryURL is the ACME directory. Empty means Let's Encrypt.
	DirectoryURL string

	// Dir is where the account key, the certificate and its key live.
	Dir string

	// Reload asks nginx to read the files just written. Without it a renewed
	// certificate sits on disk while the old one is still being served, because
	// nginx reads certificates once, at load.
	Reload func(ctx context.Context) error

	// HTTPClient is how the ACME server is reached. Nil means
	// [http.DefaultClient].
	HTTPClient *http.Client

	// Now is the clock, so that a test can hold a certificate that expires
	// tomorrow without waiting for tomorrow.
	Now func() time.Time

	Logger *slog.Logger

	// mu guards tokens, which the HTTP handler reads on a request goroutine
	// while the issuing goroutine writes it.
	mu     sync.Mutex
	tokens map[string]string
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

func (m *Manager) path(name string) string { return filepath.Join(m.Dir, name) }

// Handler answers the HTTP-01 challenge and nothing else.
//
// It is mounted at [ChallengePath] and 404s anything it was not asked to
// present, so a token that is not outstanding cannot be read back out of a
// running dokkup.
func (m *Manager) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, ChallengePath)

		m.mu.Lock()
		response, ok := m.tokens[token]
		m.mu.Unlock()

		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(response))
	})
}

func (m *Manager) present(token, response string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tokens == nil {
		m.tokens = make(map[string]string)
	}
	m.tokens[token] = response
}

func (m *Manager) forget(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tokens, token)
}

// Run keeps the certificate current until ctx is done.
//
// It issues immediately rather than waiting out the first interval, because the
// installer starts the service precisely so that this happens, and an operator
// watching an installation should not be told to come back in twelve hours.
func (m *Manager) Run(ctx context.Context) {
	for {
		wait := checkInterval

		switch issued, err := m.Ensure(ctx); {
		case err != nil && ctx.Err() != nil:
			return
		case err != nil:
			m.logger().Error("certificate not obtained; will try again",
				"domain", m.Domain, "retry_in", retryInterval, "error", err)
			wait = retryInterval
		case issued:
			m.logger().Info("certificate obtained", "domain", m.Domain)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Ensure obtains a certificate if the one on disk will not do, and reports
// whether it wrote one.
//
// Doing nothing is the ordinary answer: it is called on every start and every
// twelve hours, and a certificate with a month left is not a problem to solve.
func (m *Manager) Ensure(ctx context.Context) (bool, error) {
	if m.Domain == "" {
		return false, errors.New("acme: no domain to get a certificate for")
	}

	why, needed := m.needsCertificate()
	if !needed {
		return false, nil
	}
	m.logger().Info("obtaining a certificate", "domain", m.Domain, "because", why)

	certPEM, keyPEM, err := m.obtain(ctx)
	if err != nil {
		return false, err
	}
	if err := m.write(certPEM, keyPEM); err != nil {
		return false, err
	}

	if m.Reload != nil {
		if err := m.Reload(ctx); err != nil {
			// The certificate is written, so this is not a failed issuance and
			// must not be retried as one -- the next reload, from anything at
			// all, picks it up. It is still worth an error, because until then
			// browsers are being handed the old certificate.
			return true, fmt.Errorf("the certificate for %s was obtained and written, but nginx "+
				"did not reload, so it is still serving the previous one: %w", m.Domain, err)
		}
	}
	return true, nil
}

// needsCertificate reads what is on disk and says, in words an operator can
// read in a log, why it will not do.
func (m *Manager) needsCertificate() (string, bool) {
	certPEM, err := os.ReadFile(m.path(CertFile))
	if err != nil {
		return "there is none yet", true
	}

	leaf, err := leafOf(certPEM)
	if err != nil {
		return "the one on disk cannot be read: " + err.Error(), true
	}

	// The installer writes a self-signed certificate so that nginx has
	// something to serve while this runs. Replacing it is the whole point, and
	// it is recognised by its own signature rather than by a marker file that
	// could go missing.
	if leaf.CheckSignatureFrom(leaf) == nil {
		return "the one on disk is self-signed", true
	}
	if err := leaf.VerifyHostname(m.Domain); err != nil {
		return "the one on disk does not cover " + m.Domain, true
	}
	if left := leaf.NotAfter.Sub(m.now()); left < renewBefore {
		return fmt.Sprintf("the one on disk expires in %s", left.Round(time.Hour)), true
	}
	return "", false
}

// obtain runs one ACME order to completion and returns what to write.
func (m *Manager) obtain(ctx context.Context) (certPEM, keyPEM []byte, err error) {
	accountKey, err := m.accountKey()
	if err != nil {
		return nil, nil, err
	}

	client := &acme.Client{
		Key:          accountKey,
		DirectoryURL: m.DirectoryURL,
		HTTPClient:   m.HTTPClient,
		UserAgent:    "dokkup",
	}

	// Registering an account that exists is the ordinary path, because this
	// runs on every renewal with the same key, and the server answering "you
	// already have one" is agreement rather than failure.
	account := &acme.Account{}
	if m.Email != "" {
		account.Contact = []string{"mailto:" + m.Email}
	}
	if _, err := client.Register(ctx, account, acme.AcceptTOS); err != nil &&
		!errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, nil, fmt.Errorf("registering with the certificate authority: %w", err)
	}

	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(m.Domain))
	if err != nil {
		return nil, nil, fmt.Errorf("asking for a certificate for %s: %w", m.Domain, err)
	}

	// Held on to rather than read back off each answer. An authority conveys
	// the order's URL in the Location header when it creates the order, and RFC
	// 8555 does not ask it to repeat that header afterwards; x/crypto/acme
	// fills Order.URI from the header of whatever response it just read, so
	// polling an order is what empties the field the next poll would need.
	// Measured against Pebble 2.10.1, which sends it once and never again.
	orderURL := order.URI

	for _, url := range order.AuthzURLs {
		if err := m.satisfy(ctx, client, url); err != nil {
			return nil, nil, err
		}
	}

	order, err = client.WaitOrder(ctx, orderURL)
	if err != nil {
		return nil, nil, fmt.Errorf("waiting for %s to be authorised: %w", m.Domain, err)
	}

	// A new key every issuance. Reusing one would mean a certificate replaced
	// because its key was exposed is replaced by a certificate with the same
	// key, and generating one costs nothing on P-256.
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating a key for the certificate: %w", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		DNSNames: []string{m.Domain},
	}, certKey)
	if err != nil {
		return nil, nil, fmt.Errorf("building the certificate request: %w", err)
	}

	chain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		chain, err = m.collect(ctx, client, orderURL, err)
		if err != nil {
			return nil, nil, fmt.Errorf("collecting the certificate for %s: %w", m.Domain, err)
		}
	}

	certPEM, err = chainPEM(chain)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = privateKeyPEM(certKey)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

// collect fetches a certificate the authority may have issued already.
//
// Finalizing an order is allowed to be asynchronous, and x/crypto/acme then
// polls for the result at the URL it reads out of the finalize response's
// Location header -- which RFC 8555 does not require the authority to send.
// Measured against Pebble 2.10.1, which answers finalize with Retry-After and
// no Location: the certificate is issued, the client has nowhere to poll, and
// issuance fails with `Post "": unsupported protocol scheme ""` for a
// certificate that exists on the authority's side. Let's Encrypt does send the
// header, so this path does not run in production -- which is the reason to
// have it, because if that ever changed every renewal would stop at once and
// the certificates would already have been issued and paid for against the
// account's rate limit.
//
// There is nothing to guess: the order's own URL came back from AuthorizeOrder,
// before any of this.
func (m *Manager) collect(ctx context.Context, client *acme.Client, orderURL string,
	because error) ([][]byte, error) {
	if orderURL == "" {
		return nil, because
	}

	order, err := client.WaitOrder(ctx, orderURL)
	if err != nil {
		return nil, errors.Join(because, err)
	}
	if order.CertURL == "" {
		return nil, because
	}

	chain, err := client.FetchCert(ctx, order.CertURL, true)
	if err != nil {
		return nil, errors.Join(because, err)
	}
	m.logger().Info("the certificate was collected from the order after finalizing failed",
		"domain", m.Domain, "because", because)
	return chain, nil
}

// satisfy answers one authorization by HTTP-01.
func (m *Manager) satisfy(ctx context.Context, client *acme.Client, url string) error {
	authz, err := client.GetAuthorization(ctx, url)
	if err != nil {
		return fmt.Errorf("reading what the certificate authority wants proved: %w", err)
	}
	if authz.Status == acme.StatusValid {
		// Authorizations are reusable and outlive an order, so a renewal often
		// finds this already done.
		return nil
	}

	var challenge *acme.Challenge
	for _, offered := range authz.Challenges {
		if offered.Type == "http-01" {
			challenge = offered
			break
		}
	}
	if challenge == nil {
		return fmt.Errorf("the certificate authority did not offer an http-01 challenge for %s, "+
			"which is the only kind dokkup can answer", m.Domain)
	}

	response, err := client.HTTP01ChallengeResponse(challenge.Token)
	if err != nil {
		return fmt.Errorf("building the answer to the challenge: %w", err)
	}

	m.present(challenge.Token, response)
	defer m.forget(challenge.Token)

	if _, err := client.Accept(ctx, challenge); err != nil {
		return fmt.Errorf("telling the certificate authority to check %s: %w", m.Domain, err)
	}
	if _, err := client.WaitAuthorization(ctx, url); err != nil {
		return fmt.Errorf("the certificate authority could not reach http://%s%s<token>, which is "+
			"how it proves this host answers for that name: %w", m.Domain, ChallengePath, err)
	}
	return nil
}

// accountKey reads the account key, creating one the first time.
//
// It is kept rather than regenerated because an account is what rate limits and
// issued certificates hang off; a new key every start would look to a
// certificate authority like a new subscriber every start.
func (m *Manager) accountKey() (*ecdsa.PrivateKey, error) {
	path := m.path(AccountKeyFile)

	//nolint:gosec // beside the certificate, in dokkup's own directory
	stored, err := os.ReadFile(path)
	switch {
	case err == nil:
		block, _ := pem.Decode(stored)
		if block == nil {
			return nil, fmt.Errorf("%s is not a PEM key; remove it and dokkup will register "+
				"again", path)
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("reading the account key in %s: %w", path, err)
		}
		return key, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("reading the account key in %s: %w", path, err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating an account key: %w", err)
	}
	keyPEM, err := privateKeyPEM(key)
	if err != nil {
		return nil, err
	}
	if err := writeFile(path, keyPEM, keyMode); err != nil {
		return nil, fmt.Errorf("saving the account key to %s: %w", path, err)
	}
	return key, nil
}

// write installs the pair, key first.
//
// Order matters only for the window in between, and only one order has a safe
// one: nginx reads both files when it reloads, so a reload that landed between
// the two writes must not find a new certificate beside the old key. Writing
// the key first means the worst case is a reload finding the *old* certificate
// beside the new key, which fails to load and leaves nginx serving what it
// already had rather than serving a mismatched pair.
func (m *Manager) write(certPEM, keyPEM []byte) error {
	if err := writeFile(m.path(KeyFile), keyPEM, keyMode); err != nil {
		return fmt.Errorf("writing the private key: %w", err)
	}
	if err := writeFile(m.path(CertFile), certPEM, certMode); err != nil {
		return fmt.Errorf("writing the certificate: %w", err)
	}
	return nil
}

// Expiry reports when the certificate on disk runs out, for anything that wants
// to show it rather than act on it.
func (m *Manager) Expiry() (time.Time, error) {
	certPEM, err := os.ReadFile(m.path(CertFile))
	if err != nil {
		return time.Time{}, err
	}
	leaf, err := leafOf(certPEM)
	if err != nil {
		return time.Time{}, err
	}
	return leaf.NotAfter, nil
}

// writeFile replaces a file atomically, so that nothing ever reads half of a
// key. The temporary lands beside the target, which keeps the rename within one
// filesystem.
func writeFile(path string, content []byte, mode fs.FileMode) error {
	staging := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")

	if err := os.WriteFile(staging, content, mode); err != nil {
		return err
	}
	if err := os.Rename(staging, path); err != nil {
		_ = os.Remove(staging)
		return err
	}
	return nil
}

func leafOf(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, errors.New("no PEM certificate found")
	}
	return x509.ParseCertificate(block.Bytes)
}

func chainPEM(chain [][]byte) ([]byte, error) {
	if len(chain) == 0 {
		return nil, errors.New("the certificate authority returned an empty chain")
	}
	var out strings.Builder
	for _, der := range chain {
		if err := pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
			return nil, err
		}
	}
	return []byte(out.String()), nil
}

func privateKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}
