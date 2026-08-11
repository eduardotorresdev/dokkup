package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/dokku"
	"github.com/eduardotorresdev/dokkup/internal/host"
	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/proxy"
	"github.com/eduardotorresdev/dokkup/internal/service"
)

const (
	// installVersion is what this host's service answers as. The wait for
	// health gates on it, so a run succeeds only when what came up is what was
	// installed.
	installVersion = "1.2.3"

	// installAddress is the address this host has, and installElsewhere is
	// where a domain points when it does not point here. Both are from the
	// documentation ranges, and neither is private: a private address makes
	// installation print advice about NAT, which is another test's subject.
	installAddress   = "192.0.2.7"
	installElsewhere = "198.51.100.9"

	installDomain = "dokkup.example.com"
)

// installOps is [host.Fake] with two refinements, each needed to say
// something these tests are about and neither of them belonging in the fake.
//
// It puts the sudoers rule on disk, because [host.Tools] does, and because an
// undo that removes a file cannot be observed against a fake that never created
// one. The mode is 0600 rather than the 0440 a real host gets: these tests are
// not root, and a second installation has to be able to replace what the first
// wrote. What mode the rule is installed with belongs to host.Tools and is
// asserted there.
//
// And it keeps the candidate vhost it is asked about, because the file that
// carries it is deleted the moment the check returns.
type installOps struct {
	*host.Fake

	candidate []byte
}

var _ host.Ops = (*installOps)(nil)

func (r *installOps) WriteSudoers(ctx context.Context, path, rule string) error {
	// The fake first, so that a run told to fail here leaves nothing on disk --
	// which is what the real one does, since it validates a staged file and
	// renames only what parsed.
	if err := r.Fake.WriteSudoers(ctx, path, rule); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(path, []byte(rule), 0o600)
}

func (r *installOps) CheckProxyFragment(ctx context.Context, path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading the candidate vhost at %s: %w", path, err)
	}
	r.candidate = content
	return r.Fake.CheckProxyFragment(ctx, path)
}

// installHost is a Dokku host with nothing of dokkup's on it: an in-memory
// account database, an in-memory systemd, and a t.TempDir() standing in for /.
//
// Everything the installer reaches the world through is a field on it, so a
// test says what kind of host this is rather than what the installer ought to
// call.
type installHost struct {
	t    *testing.T
	root string

	ops     *host.Fake
	disk    *installOps
	manager *service.Fake
	inst    *installer

	stdout *bytes.Buffer
	stderr *bytes.Buffer

	// proved is the site installation went and looked for through the proxy
	// after it reloaded it, which is how a test asks what was checked rather
	// than what was intended.
	proved site
}

func newInstallHost(t *testing.T) *installHost {
	t.Helper()

	h := &installHost{
		t:       t,
		root:    t.TempDir(),
		ops:     host.NewFake(),
		manager: &service.Fake{},
		stdout:  &bytes.Buffer{},
		stderr:  &bytes.Buffer{},
	}
	h.disk = &installOps{Fake: h.ops}

	// Every one of these is on a host that has Dokku on it -- see the comment
	// on systemDirMode -- so a root without them would make each MkdirAll in
	// the installer look like a change that run made and never undid.
	for _, dir := range []string{"/usr/bin", "/usr/local/bin", "/etc/systemd/system",
		"/etc/sudoers.d", "/etc/nginx/conf.d", "/var/lib"} {
		if err := os.MkdirAll(filepath.Join(h.root, dir), systemDirMode); err != nil {
			t.Fatalf("laying out the host at %s: %v", dir, err)
		}
	}

	// Installation refuses a host with no dokku binary on it, because the
	// sudoers rule it writes would otherwise permit a program that is not
	// there.
	h.write(filepath.Join(h.root, dokku.DefaultBinary), "#!/bin/sh\nexit 0\n", binaryMode)

	// The executable installation copies. A file of its own rather than the
	// test binary, which is megabytes and would be copied once per case.
	self := filepath.Join(t.TempDir(), "dokkup")
	h.write(self, "dokkup "+installVersion, binaryMode)

	h.inst = &installer{
		env: Env{
			// Empty rather than nil: a question nobody answered has to read end
			// of input and refuse, instead of reaching for the real standard
			// input and hanging the suite.
			Stdin:  strings.NewReader(""),
			Stdout: h.stdout,
			Stderr: h.stderr,
		},
		ops:     h.disk,
		manager: h.manager,
		root:    h.root,
		self:    self,
		version: installVersion,
		cfg:     installConfig{listen: service.DefaultListen},
		// Short enough that a wait which fails is not a pause, long enough that
		// a loaded machine still gets several attempts.
		timeout:  300 * time.Millisecond,
		interval: 5 * time.Millisecond,
		health:   func(context.Context) (string, error) { return installVersion, nil },
		resolve: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP(installAddress)}, nil
		},
		local:   func() ([]net.IP, error) { return []net.IP{net.ParseIP(installAddress)}, nil },
		localIP: func() (net.IP, error) { return net.ParseIP(installAddress), nil },
	}
	h.inst.confirmProxy = func(_ context.Context, s site) error {
		h.proved = s
		return nil
	}
	return h
}

func (h *installHost) install() error { return h.inst.install(h.t.Context()) }

// answer is what the operator types, for the questions installation asks before
// it changes anything.
func (h *installHost) answer(lines string) { h.inst.env.Stdin = strings.NewReader(lines) }

func (h *installHost) write(path, content string, mode fs.FileMode) {
	h.t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), systemDirMode); err != nil {
		h.t.Fatalf("creating the directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		h.t.Fatalf("writing %s: %v", path, err)
	}
}

// read returns what is at one of dokkup's own paths on this host.
func (h *installHost) read(path string) string {
	h.t.Helper()

	content, err := os.ReadFile(filepath.Join(h.root, path))
	if err != nil {
		h.t.Fatalf("reading %s: %v", path, err)
	}
	return string(content)
}

func (h *installHost) has(path string) bool { return exists(filepath.Join(h.root, path)) }

func (h *installHost) perm(path string) fs.FileMode {
	h.t.Helper()

	info, err := os.Stat(filepath.Join(h.root, path))
	if err != nil {
		h.t.Fatalf("looking at %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// tree describes everything under this host's root: each path, what it is
// permitted to, and for a file its contents. Comparing two of them is how a
// test says the host is exactly as it was found, without writing down what
// installation would have created.
func (h *installHost) tree() map[string]string {
	h.t.Helper()

	found := map[string]string{}
	err := filepath.WalkDir(h.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(h.root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			found["/"+rel] = fmt.Sprintf("a directory at %#o", info.Mode().Perm())
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found["/"+rel] = fmt.Sprintf("%#o holding %q", info.Mode().Perm(), content)
		return nil
	})
	if err != nil {
		h.t.Fatalf("reading the tree under %s: %v", h.root, err)
	}
	return found
}

// identities renders the accounts this host has, so that "nothing was left
// behind" is one comparison of two strings rather than three loops.
func (h *installHost) identities() string {
	h.t.Helper()

	var lines []string
	for name, account := range h.ops.Users {
		lines = append(lines, fmt.Sprintf("the user %s, %d:%d", name, account.Uid, account.Gid))
	}
	for name, account := range h.ops.Groups {
		lines = append(lines, fmt.Sprintf("the group %s, %d", name, account.Gid))
	}
	for user, groups := range h.ops.Memberships {
		for _, group := range groups {
			lines = append(lines, fmt.Sprintf("%s is in %s", user, group))
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// installTreeChanges says how one tree differs from another, in prose, so that
// a restoration which missed something names the path rather than printing two
// directory listings for a person to compare.
func installTreeChanges(before, after map[string]string) []string {
	var changes []string
	for path, was := range before {
		switch now, still := after[path]; {
		case !still:
			changes = append(changes, fmt.Sprintf("%s is gone; it was %s", path, was))
		case now != was:
			changes = append(changes, fmt.Sprintf("%s is now %s; it was %s", path, now, was))
		}
	}
	for path, now := range after {
		if _, had := before[path]; !had {
			changes = append(changes, fmt.Sprintf("%s appeared, as %s", path, now))
		}
	}
	slices.Sort(changes)
	return changes
}

// installablePaths is every location an installation may put something.
//
// It is taken from hostpaths rather than written down here, so that a path
// added to what dokkup owns cannot be added to the installer and then quietly
// tolerated by a test that was told about it separately.
func installablePaths() []string {
	return slices.Concat(hostpaths.Owned(), hostpaths.Conditional(), []string{
		hostpaths.TLSCert, hostpaths.TLSKey, hostpaths.VhostStaging,
		// Dokku's own, which the host is seeded with rather than installation
		// creating it.
		dokku.DefaultBinary,
	})
}

// certificateForDomain writes a certificate and its key for name, and returns their
// paths.
//
// It is built here rather than with [proxy.SelfSigned], which puts an address
// in IPAddresses: a domain has to be in DNSNames, because that is the only
// field x509's VerifyHostname reads and installation calls it before it will
// serve a certificate at a name.
func certificateForDomain(t *testing.T, name string) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"a certificate authority"}},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("signing the certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encoding the key: %v", err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("writing the certificate: %v", err)
	}
	if err := os.WriteFile(keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("writing the key: %v", err)
	}
	return certPath, keyPath
}

func TestInstallCreatesTheUserInTheDokkuGroupAndTheDataDirectoryItCannotBeReadOutOf(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	h.inst.cfg.plainHTTP = true

	if err := h.install(); err != nil {
		t.Fatalf("install: %v", err)
	}

	account, ok := h.ops.Users[hostpaths.User]
	if !ok {
		t.Fatalf("this host has no %s account after installing", hostpaths.User)
	}
	// The membership buys reach into DOKKU_ROOT, which is 0750 dokku:dokku. It
	// is not the right to run dokku; the sudoers rule is that, on its own.
	if !slices.Contains(h.ops.Memberships[hostpaths.User], hostpaths.DokkuGroup) {
		t.Errorf("%s is in %v, and has to be in the %s group to read anything of Dokku's",
			hostpaths.User, h.ops.Memberships[hostpaths.User], hostpaths.DokkuGroup)
	}

	for _, dir := range []string{hostpaths.DataDir, hostpaths.TLSDir} {
		spec, created := h.ops.Dirs[filepath.Join(h.root, dir)]
		if !created {
			t.Fatalf("%s was never created", dir)
		}
		if spec.Mode != dataDirMode {
			t.Errorf("%s was asked for at %#o, want %#o", dir, spec.Mode, dataDirMode)
		}
		if spec.Owner != account {
			t.Errorf("%s is owned by %+v, want the %s account %+v",
				dir, spec.Owner, hostpaths.User, account)
		}
		if perm := h.perm(dir); perm&0o007 != 0 {
			t.Errorf("%s is %#o, so any other unprivileged account on this host can read the "+
				"database, the audit trail and the private key out of it", dir, perm)
		}
	}
}

func TestInstallWritesASudoersRuleForOneProgramAndNoWildcard(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	h.inst.cfg.plainHTTP = true

	if err := h.install(); err != nil {
		t.Fatalf("install: %v", err)
	}

	// One program, run as one account. The runas is Dokku's own user and not
	// root, so the ceiling on what this rule can escalate to is that account
	// rather than the whole host.
	const want = "dokkup ALL=(dokku) NOPASSWD: /usr/bin/dokku\n"

	rule, written := h.ops.Sudoers[filepath.Join(h.root, hostpaths.Sudoers)]
	if !written {
		t.Fatalf("no rule was written to %s", hostpaths.Sudoers)
	}
	if rule != want {
		t.Errorf("the rule is %q, want exactly %q", rule, want)
	}
	if strings.Contains(rule, "*") {
		t.Errorf("the rule has a wildcard in it, so it permits programs nobody chose: %q", rule)
	}
	if strings.Contains(rule, "ALL)") {
		t.Errorf("the rule runs as anyone, root included, which is the whole host rather than "+
			"the dokku account: %q", rule)
	}
}

func TestInstallProvesTheSudoersRuleRatherThanAssumingIt(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	h.inst.cfg.plainHTTP = true
	h.ops.FailAt = "prove-sudo"

	err := h.install()

	if err == nil {
		t.Fatal("an installation whose sudoers rule does not work reported success")
	}
	// A rule with the wrong mode, the wrong owner or the wrong runas is accepted
	// without complaint and fails later, from inside a service, as a health
	// endpoint reporting that this host has no applications on it.
	if slices.Contains(h.manager.Calls, "enable") {
		t.Errorf("the service was enabled although %s cannot reach dokku: %v",
			hostpaths.User, h.manager.Calls)
	}
	if !strings.Contains(h.stderr.String(), "Putting this host back the way it was found") {
		t.Errorf("the operator was not told the host was being put back: %q", h.stderr.String())
	}
}

func TestAnInstallThatFailsPartWayLeavesTheHostAsItFoundIt(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		// failAt is the host operation that refuses, named as the fake records
		// it.
		failAt string

		// managerErr and healthErr are the two failures that arrive from
		// somewhere other than the host's own programs: systemd refusing the
		// unit, and the service never answering. Both are late, which makes
		// them the deepest unwinding there is.
		managerErr error
		healthErr  error
	}{
		"the group cannot be created":                           {failAt: "ensure-group"},
		"the account cannot be created":                         {failAt: "ensure-user"},
		"the data directory cannot be created":                  {failAt: "ensure-dir"},
		"the sudoers rule cannot be written":                    {failAt: "write-sudoers"},
		"the sudoers rule does not let the service reach dokku": {failAt: "prove-sudo"},
		"the vhost does not pass nginx's check on its own":      {failAt: "check-proxy-fragment"},
		"this host's nginx configuration was already failing":   {failAt: "check-proxy"},
		"systemd will not read the unit":                        {managerErr: errors.New("Failed to reload daemon")},
		"the service never answers":                             {healthErr: errors.New("connection refused")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newInstallHost(t)
			h.inst.cfg.plainHTTP = true

			before, accounts := h.tree(), h.identities()

			h.ops.FailAt = tc.failAt
			h.manager.Err = tc.managerErr
			if tc.healthErr != nil {
				h.inst.health = func(context.Context) (string, error) { return "", tc.healthErr }
			}

			if err := h.install(); err == nil {
				t.Fatal("an installation that could not finish reported success")
			}

			if changes := installTreeChanges(before, h.tree()); len(changes) > 0 {
				t.Errorf("this host is not as it was found:\n  %s", strings.Join(changes, "\n  "))
			}
			if got := h.identities(); got != accounts {
				t.Errorf("the accounts on this host are\n%s\nand were\n%s", got, accounts)
			}

			// The fake's Dirs and Sudoers are records of what was asked for
			// rather than of what this host has, so what became of them is
			// asked of the filesystem: an undo removes the directory and the
			// file, and has no reason to remove the record of the request.
			for path := range h.ops.Dirs {
				if exists(path) {
					t.Errorf("%s was created by a run that failed, and is still there", path)
				}
			}
			if h.has(hostpaths.Sudoers) {
				t.Errorf("%s was left behind by a run that failed, so this host permits "+
					"something no installation of dokkup is here to use", hostpaths.Sudoers)
			}
		})
	}
}

// The test that stops a failed re-run destroying a working installation: an
// installation which finds the account and the database already there has not
// created them, and unwinding must not take them.
func TestAnInstallThatFindsWhatItWouldHaveCreatedRemovesNoneOfItWhenItFails(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	h.inst.cfg.plainHTTP = true

	h.ops.Users[hostpaths.User] = host.Account{Name: hostpaths.User, Uid: 998, Gid: 998}
	h.ops.Groups[hostpaths.Group] = host.Account{Name: hostpaths.Group, Uid: -1, Gid: 998}
	const operators = "the operators, the audit trail and the deploy history"
	h.write(filepath.Join(h.root, hostpaths.DataDir, "dokkup.db"), operators, 0o600)

	// Late, so that everything this run could create has been created by the
	// time it gives up.
	h.inst.health = func(context.Context) (string, error) {
		return "", errors.New("connection refused")
	}

	if err := h.install(); err == nil {
		t.Fatal("an installation whose service never answered reported success")
	}

	if _, ok := h.ops.Users[hostpaths.User]; !ok {
		t.Errorf("the %s account was removed by a run that did not create it", hostpaths.User)
	}
	if _, ok := h.ops.Groups[hostpaths.Group]; !ok {
		t.Errorf("the %s group was removed by a run that did not create it", hostpaths.Group)
	}
	if !h.has(hostpaths.DataDir) {
		t.Fatalf("%s was removed by a run that found it already there", hostpaths.DataDir)
	}
	if got := h.read(filepath.Join(hostpaths.DataDir, "dokkup.db")); got != operators {
		t.Errorf("the database now holds %q, and held %q", got, operators)
	}

	// What this run did create still goes, or a failed installation would be a
	// way of accumulating half-installations.
	if h.has(hostpaths.TLSDir) {
		t.Errorf("%s was created by this run and left behind by it", hostpaths.TLSDir)
	}
	if h.has(hostpaths.Binary) {
		t.Errorf("%s was created by this run and left behind by it", hostpaths.Binary)
	}
}

func TestInstallingTwiceChangesNothingTheSecondTime(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	h.inst.cfg.plainHTTP = true

	if err := h.install(); err != nil {
		t.Fatalf("the first install: %v", err)
	}
	first, accounts := h.tree(), h.identities()

	// A data directory an earlier run left readable by the whole host is what
	// re-running is expected to repair rather than to detect.
	if err := os.Chmod(filepath.Join(h.root, hostpaths.DataDir), 0o777); err != nil {
		t.Fatalf("loosening %s: %v", hostpaths.DataDir, err)
	}

	again := *h.inst
	again.undo = nil
	again.env.Stdin = strings.NewReader("")
	if err := again.install(t.Context()); err != nil {
		t.Fatalf("the second install: %v", err)
	}

	if got := h.perm(hostpaths.DataDir); got != dataDirMode {
		t.Errorf("%s is %#o after a second installation, want it put back to %#o",
			hostpaths.DataDir, got, dataDirMode)
	}
	if got := h.identities(); got != accounts {
		t.Errorf("the second installation changed the accounts on this host to\n%s\nfrom\n%s",
			got, accounts)
	}

	// The one thing a second installation adds is a copy of the executable it
	// replaced. That is the same act as an update keeping one, and there is no
	// reason for two slots.
	after := h.tree()
	delete(after, hostpaths.PreviousBinary)
	if changes := installTreeChanges(first, after); len(changes) > 0 {
		t.Errorf("the second installation was not a no-op:\n  %s", strings.Join(changes, "\n  "))
	}
	if got, want := h.read(hostpaths.PreviousBinary), h.read(hostpaths.Binary); got != want {
		t.Errorf("the copy kept at %s holds %q, and the binary it was taken from holds %q",
			hostpaths.PreviousBinary, got, want)
	}
}

func TestInstallRefusesADomainItCannotGetACertificateFor(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	h.inst.cfg.domain = installDomain
	before := h.tree()

	err := h.install()

	if err == nil {
		t.Fatal("installation went ahead at a domain with no certificate to serve there")
	}
	// Dokku's Let's Encrypt plugin issues certificates for apps, and dokkup is
	// not an app, so the refusal has to say what the operator can do instead.
	if !strings.Contains(err.Error(), "--cert") {
		t.Errorf("the refusal does not name the flag that would work: %v", err)
	}
	if !strings.Contains(err.Error(), "install by IP address") {
		t.Errorf("the refusal does not offer installing at this host's address: %v", err)
	}

	if changes := installTreeChanges(before, h.tree()); len(changes) > 0 {
		t.Errorf("a refused installation wrote:\n  %s", strings.Join(changes, "\n  "))
	}
	if _, ok := h.ops.Users[hostpaths.User]; ok {
		t.Errorf("a refused installation created the %s account", hostpaths.User)
	}
	if len(h.ops.Dirs) != 0 || len(h.ops.Sudoers) != 0 {
		t.Errorf("a refused installation asked for %v and %v", h.ops.Dirs, h.ops.Sudoers)
	}
}

// A name that resolves nowhere can reach nothing and can never be validated by
// a certificate authority, so installing at it would be installing at nowhere.
func TestADomainThatDoesNotResolveIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	cert, key := certificateForDomain(t, installDomain)
	h.inst.cfg.domain, h.inst.cfg.cert, h.inst.cfg.key = installDomain, cert, key
	h.inst.resolve = func(context.Context, string) ([]net.IP, error) {
		return nil, &net.DNSError{Err: "no such host", Name: installDomain, IsNotFound: true}
	}
	before := h.tree()

	err := h.install()

	if err == nil {
		t.Fatal("a name that resolves nowhere was installed at")
	}
	if !strings.Contains(err.Error(), "does not resolve at all") {
		t.Errorf("the refusal does not say what is wrong with the name: %v", err)
	}
	if changes := installTreeChanges(before, h.tree()); len(changes) > 0 {
		t.Errorf("a refused installation wrote:\n  %s", strings.Join(changes, "\n  "))
	}
	if _, ok := h.ops.Users[hostpaths.User]; ok {
		t.Errorf("a refused installation created the %s account", hostpaths.User)
	}
}

// A name that resolves somewhere else is the ordinary answer behind NAT, which
// is every AWS, GCP and Azure instance ever created. Refusing it would refuse
// most cloud hosts, so it is a question rather than a refusal.
func TestADomainThatResolvesElsewhereIsAWarningRatherThanARefusal(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	cert, key := certificateForDomain(t, installDomain)
	h.inst.cfg.domain, h.inst.cfg.cert, h.inst.cfg.key = installDomain, cert, key
	h.inst.resolve = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP(installElsewhere)}, nil
	}
	h.answer("yes\n")

	if err := h.install(); err != nil {
		t.Fatalf("an installation the operator confirmed was refused anyway: %v", err)
	}

	asked := h.stdout.String()
	if !strings.Contains(asked, installElsewhere) || !strings.Contains(asked, "NAT") {
		t.Errorf("the operator was not told where the name points, or why that is ordinary: %q",
			asked)
	}
	supplied, err := os.ReadFile(cert)
	if err != nil {
		t.Fatalf("reading the certificate this test supplied: %v", err)
	}
	if got := h.read(hostpaths.TLSCert); got != string(supplied) {
		t.Errorf("what is at %s is not the certificate the operator supplied", hostpaths.TLSCert)
	}
	if vhost := h.read(hostpaths.Vhost); !strings.Contains(vhost, installDomain) {
		t.Errorf("the vhost does not serve %s:\n%s", installDomain, vhost)
	}
}

func TestIPModeOffersASelfSignedCertificateAndPrintsTheFingerprintThatIsServed(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	h.answer("yes\n")

	if err := h.install(); err != nil {
		t.Fatalf("install: %v", err)
	}

	block, _ := pem.Decode([]byte(h.read(hostpaths.TLSCert)))
	if block == nil {
		t.Fatalf("what was written to %s is not a certificate", hostpaths.TLSCert)
	}
	want := proxy.Fingerprint(block.Bytes)

	// The number the operator compares in the browser has to be the number of
	// the certificate this host actually serves. A fingerprint of anything else
	// is stable, plausible and equal to nothing they can see.
	if !strings.Contains(h.stdout.String(), want) {
		t.Errorf("the fingerprint printed is not %s, which is the certificate at %s: %q",
			want, hostpaths.TLSCert, h.stdout.String())
	}
	if h.proved.fingerprint != want {
		t.Errorf("installation went and checked for %s, having installed %s",
			h.proved.fingerprint, want)
	}
	if got := h.perm(hostpaths.TLSKey); got != keyMode {
		t.Errorf("the private key is %#o, want %#o", got, keyMode)
	}
}

// Someone pressing return to be rid of a prompt has chosen nothing, and this
// question precedes changes to a host.
func TestWithNoAnswerToReadInstallRefusesRatherThanChoosingForTheOperator(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	before := h.tree()

	err := h.install()

	if err == nil {
		t.Fatal("installation chose for the operator and carried on")
	}
	if !strings.Contains(err.Error(), "--accept-self-signed") ||
		!strings.Contains(err.Error(), "--plain-http") {
		t.Errorf("the refusal does not name the flags that answer the question: %v", err)
	}
	if changes := installTreeChanges(before, h.tree()); len(changes) > 0 {
		t.Errorf("an unanswered question wrote:\n  %s", strings.Join(changes, "\n  "))
	}
}

// The mode is what makes the server restrict itself to the owner and say so on
// every screen, so it must follow what a browser will actually trust rather
// than whether a certificate exists.
func TestTheUnitSaysPublishedOnlyWhenABrowserWillTrustTheCertificate(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		configure func(t *testing.T, h *installHost)
		want      string
	}{
		"an address, with a certificate dokkup signed itself": {
			configure: func(_ *testing.T, h *installHost) { h.inst.cfg.selfSigned = true },
			want:      "--mode ip",
		},
		"an address, over plain HTTP": {
			configure: func(_ *testing.T, h *installHost) { h.inst.cfg.plainHTTP = true },
			want:      "--mode ip",
		},
		"a domain, with a certificate the operator supplied": {
			configure: func(t *testing.T, h *installHost) {
				cert, key := certificateForDomain(t, installDomain)
				h.inst.cfg.domain, h.inst.cfg.cert, h.inst.cfg.key = installDomain, cert, key
			},
			want: "--mode published",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newInstallHost(t)
			tc.configure(t, h)

			if err := h.install(); err != nil {
				t.Fatalf("install: %v", err)
			}

			unit := h.read(hostpaths.Unit)
			if !strings.Contains(unit, tc.want) {
				t.Errorf("the unit does not say %q:\n%s", tc.want, unit)
			}
			if tc.want == "--mode ip" && strings.Contains(unit, "published") {
				t.Errorf("the unit claims dokkup is published at a name a browser will trust:\n%s",
					unit)
			}
		})
	}
}

// nginx.service carries an ExecStartPre of `nginx -t -q`, so a vhost that fails
// its check does not break a reload -- it breaks the next start, which takes
// every app on the host down at the next reboot. Installing bytes other than
// the ones that passed would be the same failure with an extra step.
func TestTheVhostWrittenIsTheOneThatWasTested(t *testing.T) {
	t.Parallel()

	h := newInstallHost(t)
	h.inst.cfg.plainHTTP = true

	if err := h.install(); err != nil {
		t.Fatalf("install: %v", err)
	}

	if len(h.disk.candidate) == 0 {
		t.Fatal("nothing was tested before a vhost was put into nginx's configuration")
	}
	if got, want := h.read(hostpaths.Vhost), string(h.disk.candidate); got != want {
		t.Errorf("the vhost nginx was asked about is not the one installed:\n"+
			"--- asked about ---\n%s\n--- installed ---\n%s", want, got)
	}

	// nginx.conf globs conf.d/*.conf, and this name is what keeps a vhost that
	// is still being written out of the live configuration.
	if h.has(hostpaths.VhostStaging) {
		t.Errorf("%s is still there after a finished installation", hostpaths.VhostStaging)
	}
	if candidate := filepath.Join(hostpaths.DataDir, "vhost.candidate"); h.has(candidate) {
		t.Errorf("%s is still there after a finished installation", candidate)
	}
}

// Installation is given a root so that these tests can watch it, and the root
// is worth nothing if anything reaches around it. Both halves are asserted: no
// real location changed, and everything written under the root is a path
// hostpaths names.
func TestInstallationWritesNothingOutsideTheRootItWasGiven(t *testing.T) {
	t.Parallel()

	// What this machine has at dokkup's real locations, it must still have.
	// Asserting they are absent would be a different claim, and a false one on
	// a machine that has dokkup installed on it.
	outside := map[string]bool{}
	for _, path := range installablePaths() {
		outside[path] = exists(path)
	}

	h := newInstallHost(t)
	h.answer("yes\n")

	if err := h.install(); err != nil {
		t.Fatalf("install: %v", err)
	}

	for path, was := range outside {
		if now := exists(path); now != was {
			t.Errorf("installation reached outside the root it was given: %s was on this machine "+
				"(%v) and is now (%v)", path, was, now)
		}
	}

	allowed := map[string]bool{}
	for _, path := range installablePaths() {
		for at := path; at != "/" && at != "."; at = filepath.Dir(at) {
			allowed[at] = true
		}
	}
	for path := range h.tree() {
		if !allowed[path] {
			t.Errorf("%s was written on this host and is not a path hostpaths names", path)
		}
	}
}
