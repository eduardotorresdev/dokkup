package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/dokku"
	"github.com/eduardotorresdev/dokkup/internal/host"
	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/proxy"
	"github.com/eduardotorresdev/dokkup/internal/server"
	"github.com/eduardotorresdev/dokkup/internal/service"
)

const (
	// dataDirMode is what /var/lib/dokkup is created as. Not 0755: the
	// database, the audit trail and any private key live in there, so another
	// unprivileged account on the host must not even be able to list it.
	dataDirMode = 0o750

	// readableMode is for the files something other than dokkup has to read --
	// systemd its unit, nginx its vhost and certificate.
	readableMode = 0o644

	// keyMode is for the private key. nginx reads it as root before it drops
	// privileges, so nothing else needs to.
	keyMode = 0o600

	// binaryMode is what the executable is installed as.
	binaryMode = 0o755

	// nonexistentHome is the service account's home directory, which is
	// deliberately not created. dokkup keeps its state in [hostpaths.DataDir]
	// and an account with nowhere to write is one fewer place for state to
	// accumulate unnoticed.
	nonexistentHome = "/nonexistent"

	// nginxUnit is the service that owns the proxy dokkup writes one file into.
	nginxUnit = "nginx"

	// certLifetime is how long a self-signed certificate is good for. Ten
	// years, because nothing renews it: an operator who is shown a fingerprint
	// once should not be shown a new one in a year for no reason, and the
	// honest fix for IP mode is publishing at a domain rather than a shorter
	// certificate nobody is watching the expiry of.
	certLifetime = 10 * 365 * 24 * time.Hour

	// proxyConfirmTimeout and proxyConfirmInterval bound the check that the
	// reload took. `systemctl reload nginx` returns before the new
	// configuration is answering -- measured, a vhost removed before the reload
	// was still serving 200 the instant reload exited 0 and gone two seconds
	// later -- so this asks the proxy rather than trusting the exit status.
	proxyConfirmTimeout  = 10 * time.Second
	proxyConfirmInterval = 500 * time.Millisecond

	// dnsTimeout bounds the check that a domain points here. A resolver that is
	// not answering is a question for the operator, not a reason to hang.
	dnsTimeout = 5 * time.Second
)

// certificateWaitTimeout bounds how long installation watches for the
// certificate the service is getting.
//
// It is generous because what is being waited on is a stranger on the internet
// making an HTTP request to this host, and the failures worth distinguishing --
// a firewall on port 80, a name that resolves elsewhere -- look exactly like
// slowness until they time out. Running out is not a failed installation: the
// service goes on trying, and internal/acme retries every fifteen minutes.
const certificateWaitTimeout = 90 * time.Second

func runInstall(env Env, args []string) error {
	fs := newFlagSet(env, "install")
	domain := fs.String("domain", "", "publish dokkup at this domain")
	cert := fs.String("cert", "", "certificate chain to serve at --domain, PEM; "+
		"omit to have dokkup obtain one")
	key := fs.String("key", "", "private key for --cert, PEM")
	acmeEmail := fs.String("acme-email", "",
		"contact address the certificate authority warns about expiry at")
	acmeDirectory := fs.String("acme-directory", "",
		"ACME directory URL; empty means Let's Encrypt")
	ip := fs.String("ip", "", "address to serve at in IP mode (default: this host's own)")
	selfSigned := fs.Bool("accept-self-signed", false,
		"in IP mode, generate a self-signed certificate without asking")
	plainHTTP := fs.Bool("plain-http", false,
		"in IP mode, serve plain HTTP instead of offering a certificate")
	listen := fs.String("listen", service.DefaultListen, "address the service binds")
	timeout := fs.Duration("timeout", defaultHealthTimeout,
		"how long the service has to answer as healthy")

	if err := parseFlags(fs, args); err != nil {
		return err
	}

	cfg := installConfig{
		domain:        *domain,
		cert:          *cert,
		key:           *key,
		acmeEmail:     *acmeEmail,
		acmeDirectory: *acmeDirectory,
		ip:            *ip,
		selfSigned:    *selfSigned,
		plainHTTP:     *plainHTTP,
		listen:        *listen,
	}

	// The plan is printed before the refusals below rather than after them, so
	// that someone who runs `dokkup install` without sudo to find out what it
	// would do gets the answer as well as the refusal.
	printInstallPlan(env, cfg)

	if err := installPreflight(); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding this executable in order to install it: %w", err)
	}

	inst := &installer{
		env:      env,
		ops:      host.NewTools(),
		manager:  service.NewSystemctl(),
		self:     self,
		version:  env.Build.Version,
		cfg:      cfg,
		timeout:  *timeout,
		interval: healthPollInterval,
		resolve:  resolveDomain,
		local:    localAddresses,
		localIP:  proxy.LocalIP,
	}

	client := &http.Client{Timeout: healthProbeTimeout}
	// Read back out of the unit this run installs rather than from the flag,
	// for the reason `update` does it: the unit is the authority on how the
	// service runs, and an operator who edited it should be waited for at the
	// address they actually bound. It is evaluated when the wait happens, by
	// which time the unit is on disk.
	inst.health = func(ctx context.Context) (string, error) {
		return probeHealth(ctx, client, healthURLFromUnit(hostpaths.Unit))
	}
	inst.confirmProxy = func(ctx context.Context, s site) error {
		probe, url := proxyProbe(s)
		return waitHealthy(ctx, func(ctx context.Context) (string, error) {
			return probeHealth(ctx, probe, url)
		}, inst.version, proxyConfirmTimeout, proxyConfirmInterval)
	}

	return inst.install(context.Background())
}

// installPreflight refuses an installation this host cannot carry out, before
// anything has been changed and while saying so still costs nothing.
func installPreflight() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("dokkup installs on Linux hosts; this is %s", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		return errors.New("run as root: this creates a system user, writes a sudoers rule " +
			"and installs a system service")
	}
	return nil
}

// installConfig is the flags, as given.
type installConfig struct {
	domain        string
	cert          string
	key           string
	acmeEmail     string
	acmeDirectory string
	ip            string
	selfSigned    bool
	plainHTTP     bool
	listen        string
}

// site is how this installation will be reached, resolved from the flags and
// from what the host can be asked.
type site struct {
	how    proxy.Reachability
	domain string
	ip     net.IP

	// certPEM and keyPEM are what goes into [hostpaths.TLSDir]. Both are nil
	// for plain HTTP.
	certPEM []byte
	keyPEM  []byte

	// fingerprint is set only for a certificate dokkup generated, which is the
	// only one an operator has to check by eye.
	fingerprint string

	// manageCert says the certificate on disk is a placeholder and the service
	// is to replace it with one from a certificate authority.
	manageCert bool

	// mode is what the unit tells the server about how it is reached.
	mode server.Mode

	// url is where to tell the operator to go.
	url string
}

// undoStep is one thing this run changed and how to change it back.
type undoStep struct {
	what string
	run  func(ctx context.Context) error
}

// installer performs one installation. Everything it touches the outside world
// through is a field, so the whole of it -- including the unwinding of a run
// that fails half way -- is exercised by tests that need neither root, nor
// systemd, nor nginx, nor a Dokku host.
type installer struct {
	env     Env
	ops     host.Ops
	manager service.Manager

	// health reports which dokkup answered the health endpoint.
	health func(ctx context.Context) (string, error)

	// confirmProxy reports whether dokkup can be reached the way this
	// installation says it can, through nginx rather than on the loopback
	// address the service binds.
	confirmProxy func(ctx context.Context, s site) error

	// resolve is the DNS lookup the domain check uses, and local reports the
	// addresses this host has. Both are fields because the check has an
	// opinion about their answers and that opinion is what is worth testing.
	resolve func(ctx context.Context, name string) ([]net.IP, error)
	local   func() ([]net.IP, error)
	localIP func() (net.IP, error)

	// root is prefixed to every path dokkup writes. Empty is the real host;
	// tests pass a t.TempDir().
	root string

	// self is the executable to install.
	self string

	// version is what the service must answer as before this is a success.
	version string

	cfg      installConfig
	timeout  time.Duration
	interval time.Duration

	// certWait bounds the watch for the certificate the service is getting.
	// Zero means [certificateWaitTimeout]; it is a field for the reason timeout
	// is one, which is that ninety seconds of a test is ninety seconds.
	certWait time.Duration

	// undo is what this run has changed, newest last. A step is appended only
	// once the change it undoes has actually happened, and never for something
	// that was already here.
	undo []undoStep
}

func (i *installer) path(p string) string { return hostpaths.Rooted(i.root, p) }

func (i *installer) undoing(what string, run func(ctx context.Context) error) {
	i.undo = append(i.undo, undoStep{what: what, run: run})
}

// install does the whole of it, and leaves the host as it found it if it
// cannot.
func (i *installer) install(ctx context.Context) error {
	if err := i.requireDokku(ctx); err != nil {
		return err
	}

	s, err := i.resolveSite(ctx)
	if err != nil {
		return err
	}

	// Before the first change. A configuration this host already had broken is
	// reported as such rather than discovered half way through an installation
	// and blamed on dokkup -- and dokkup must not bury it either, because it is
	// about to add a file of its own to the same configuration.
	if err := i.ops.CheckProxy(ctx); err != nil {
		return fmt.Errorf("this host's nginx configuration was already failing its own check "+
			"before dokkup wrote anything, so dokkup has written nothing; fix that first: %w", err)
	}

	if err := i.change(ctx, s); err != nil {
		i.unwind(ctx)
		return err
	}

	i.report(s)
	return nil
}

// change performs the steps that alter the host, in the order they have to
// happen in.
func (i *installer) change(ctx context.Context, s site) error {
	account, err := i.ensureAccount(ctx)
	if err != nil {
		return err
	}
	if err := i.ensureDataDir(ctx, account); err != nil {
		return err
	}
	if err := i.ensureSudoers(ctx); err != nil {
		return err
	}
	if err := i.installBinary(); err != nil {
		return err
	}
	if err := i.installCertificate(s); err != nil {
		return err
	}
	if err := i.installVhost(ctx, s); err != nil {
		return err
	}
	if err := i.installUnit(ctx, s); err != nil {
		return err
	}
	// nginx reads the new file before the service starts, rather than after.
	//
	// It has to be this way round for an installation that gets its own
	// certificate: the service asks a certificate authority the moment it comes
	// up, and the authority proves the name by fetching a token through this
	// very vhost. Measured on the dev environment with the order reversed --
	// the first attempt failed against Dokku's catch-all, which answers
	// `return 444`, and the operator waited fifteen minutes for the retry on a
	// host where nothing was actually wrong.
	//
	// What it costs is the moment between the reload and the service binding,
	// where the proxy answers 502 to a request nobody has made yet.
	if err := i.reloadProxy(ctx); err != nil {
		return err
	}
	if err := i.startService(ctx); err != nil {
		return err
	}
	if err := i.confirmSite(ctx, s); err != nil {
		return err
	}

	// After the proxy, and deliberately not part of it. The certificate
	// authority reaches this host through nginx, so there is nothing to wait for
	// until nginx is serving; and a certificate that does not arrive is not a
	// failed installation, so this returns no error and unwinds nothing.
	if s.manageCert {
		i.awaitCertificate(ctx)
	}
	return nil
}

// awaitCertificate watches for the service to replace the placeholder.
//
// It reports rather than decides. Everything it is waiting on is outside this
// host -- DNS, a firewall, a certificate authority's queue -- and an
// installation that tore itself down over any of them would be undoing the
// work that has to exist before the next attempt can succeed. So a run that
// times out has still installed dokkup, and says exactly what is and is not
// true.
func (i *installer) awaitCertificate(ctx context.Context) {
	printf(i.env.Stdout, "Waiting for a certificate for %s...\n", i.cfg.domain)

	wait := i.certWait
	if wait <= 0 {
		wait = certificateWaitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	for {
		if issuer, ok := i.issuedCertificate(); ok {
			printf(i.env.Stdout, "%s issued a certificate for %s.\n", issuer, i.cfg.domain)
			return
		}
		select {
		case <-ctx.Done():
			printf(i.env.Stderr,
				"\n  No certificate has arrived yet, and dokkup is serving a self-signed one\n"+
					"  in the meantime, so a browser will warn. The service goes on trying every\n"+
					"  fifteen minutes; `journalctl -u %s` says why each attempt failed.\n"+
					"  The usual causes are port 80 closed to the internet, or %s\n"+
					"  resolving somewhere other than this host.\n",
				service.Name, i.cfg.domain)
			return
		case <-time.After(i.interval):
		}
	}
}

// issuedCertificate reports who signed the certificate on disk, and whether
// anyone but this host did.
//
// Read off the file rather than from the service, because the file is what
// nginx serves and what the operator is being told about. A self-signed
// certificate is recognised by its own signature, exactly as [acme.Manager]
// recognises it, so the two cannot disagree about whether the job is done.
func (i *installer) issuedCertificate() (string, bool) {
	certPEM, err := os.ReadFile(i.path(hostpaths.TLSCert))
	if err != nil {
		return "", false
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", false
	}
	if leaf.CheckSignatureFrom(leaf) == nil {
		return "", false
	}

	issuer := leaf.Issuer.CommonName
	if len(leaf.Issuer.Organization) > 0 {
		issuer = leaf.Issuer.Organization[0]
	}
	return issuer, true
}

// requireDokku refuses a host that does not have Dokku on it.
//
// Three things are asked rather than one, because they fail apart: a host can
// have the group and not the binary if Dokku was removed by hand, and the
// sudoers rule below names a program that has to exist or it permits nothing.
func (i *installer) requireDokku(ctx context.Context) error {
	if _, err := i.ops.LookupGroup(ctx, hostpaths.DokkuGroup); err != nil {
		if errors.Is(err, host.ErrNoSuchGroup) {
			return fmt.Errorf("this host does not have Dokku on it: there is no %q group to "+
				"join; install Dokku first", hostpaths.DokkuGroup)
		}
		return err
	}
	if _, err := i.ops.LookupUser(ctx, dokku.DefaultRunAs); err != nil {
		if errors.Is(err, host.ErrNoSuchUser) {
			return fmt.Errorf("this host does not have Dokku on it: there is no %q account to "+
				"run dokku as; install Dokku first", dokku.DefaultRunAs)
		}
		return err
	}
	if _, err := os.Stat(i.path(dokku.DefaultBinary)); err != nil {
		return fmt.Errorf("dokkup expects Dokku's binary at %s and there is nothing there, so "+
			"the sudoers rule it writes would permit a program that does not exist: %w",
			dokku.DefaultBinary, err)
	}
	return nil
}

// resolveSite works out how dokkup will be reached, asking wherever the plan
// said it would ask. Nothing has been written when it returns, so a refusal
// here costs the operator nothing.
func (i *installer) resolveSite(ctx context.Context) (site, error) {
	if i.cfg.domain != "" {
		if i.cfg.selfSigned || i.cfg.plainHTTP || i.cfg.ip != "" {
			return site{}, errors.New("--ip, --accept-self-signed and --plain-http are for " +
				"installing at an address; --domain is for installing at a name")
		}
		return i.resolveDomainSite(ctx)
	}
	return i.resolveIPSite()
}

func (i *installer) resolveDomainSite(ctx context.Context) (site, error) {
	switch {
	case i.cfg.cert == "" && i.cfg.key == "":
		return i.resolveManagedDomainSite(ctx)
	case i.cfg.cert == "" || i.cfg.key == "":
		return site{}, errors.New("--cert and --key go together: pass both to serve a " +
			"certificate you have, or neither to have dokkup obtain one")
	}

	//nolint:gosec // the operator names the certificate they want installed
	certPEM, err := os.ReadFile(i.cfg.cert)
	if err != nil {
		return site{}, fmt.Errorf("reading the certificate: %w", err)
	}
	//nolint:gosec // and its key
	keyPEM, err := os.ReadFile(i.cfg.key)
	if err != nil {
		return site{}, fmt.Errorf("reading the private key: %w", err)
	}

	// Paired here rather than left to nginx. A key that does not belong to its
	// certificate fails at `nginx -t` with a message about a file, halfway
	// through an installation; failing on it now costs nothing.
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return site{}, fmt.Errorf("the certificate in %s and the key in %s do not go together: %w",
			i.cfg.cert, i.cfg.key, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return site{}, fmt.Errorf("reading the certificate in %s: %w", i.cfg.cert, err)
	}
	if err := leaf.VerifyHostname(i.cfg.domain); err != nil {
		return site{}, fmt.Errorf("the certificate in %s does not cover %s, so a browser would "+
			"refuse it: %w", i.cfg.cert, i.cfg.domain, err)
	}

	if err := i.checkDomainResolves(ctx, i.cfg.domain); err != nil {
		return site{}, err
	}

	return site{
		how:     proxy.AtDomain,
		domain:  i.cfg.domain,
		certPEM: certPEM,
		keyPEM:  keyPEM,
		mode:    server.ModePublished,
		url:     "https://" + i.cfg.domain + "/",
	}, nil
}

// resolveManagedDomainSite is `--domain` with no certificate supplied: dokkup
// gets its own.
//
// What it puts on disk is a placeholder, and that is the whole shape of the
// thing. The certificate cannot be obtained before the service is running,
// because a certificate authority proves this host answers for the name by
// fetching a token from it over port 80; and nginx will not load a `listen 443
// ssl` block whose certificate file is missing. So installation writes a
// self-signed certificate, starts the service, and the service replaces it.
// The paths never change, so nothing rewrites the vhost.
//
// The DNS check still runs, and here it earns much more than it did when the
// operator supplied a certificate: a name that does not resolve cannot be
// validated by anyone, so refusing now saves an installation that would have
// spent ninety seconds failing.
func (i *installer) resolveManagedDomainSite(ctx context.Context) (site, error) {
	if err := i.checkDomainResolves(ctx, i.cfg.domain); err != nil {
		return site{}, err
	}

	certPEM, keyPEM, err := proxy.SelfSignedForDomain(i.cfg.domain, time.Now().Add(certLifetime))
	if err != nil {
		return site{}, fmt.Errorf("making a certificate to serve until the real one arrives: %w", err)
	}

	return site{
		how:        proxy.AtDomain,
		domain:     i.cfg.domain,
		certPEM:    certPEM,
		keyPEM:     keyPEM,
		manageCert: true,
		mode:       server.ModePublished,
		url:        "https://" + i.cfg.domain + "/",
	}, nil
}

// checkDomainResolves refuses a name that cannot possibly work and asks about
// one that merely looks wrong.
//
// The distinction is the whole of it. A name that does not resolve at all can
// reach nothing and can never be validated by a certificate authority, so
// installing at it would be installing at nowhere. A name that resolves
// somewhere else is the ordinary answer behind NAT, which is every AWS, GCP and
// Azure instance ever created, and refusing it would refuse most cloud hosts.
func (i *installer) checkDomainResolves(ctx context.Context, domain string) error {
	addrs, err := i.resolve(ctx, domain)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return fmt.Errorf("%s does not resolve at all, so nothing could reach dokkup there "+
				"and no certificate authority could ever validate it; point the name at this "+
				"host first, or install at this host's address and publish later", domain)
		}
		return i.mustConfirm(
			fmt.Sprintf("%s could not be looked up: %v\n"+
				"A resolver that is not answering says nothing about the name itself.", domain, err),
			"Continue anyway?")
	}

	local, err := i.local()
	if err != nil {
		return fmt.Errorf("reading this host's own addresses in order to check %s: %w", domain, err)
	}
	for _, addr := range addrs {
		for _, mine := range local {
			if addr.Equal(mine) {
				return nil
			}
		}
	}

	return i.mustConfirm(fmt.Sprintf(
		"%s resolves to %s, which is not an address this host has.\n"+
			"That is the ordinary answer behind NAT: a cloud instance sees a private\n"+
			"address while its public record points at the router in front of it. Note\n"+
			"also that /etc/hosts is consulted before DNS, so an entry there can make\n"+
			"this check agree when public DNS would not -- and that nothing checked on\n"+
			"this host can prove the name reaches it from outside. Only a certificate\n"+
			"authority's own validation can.", domain, joinIPs(addrs)),
		"Continue anyway?")
}

func (i *installer) resolveIPSite() (site, error) {
	if i.cfg.selfSigned && i.cfg.plainHTTP {
		return site{}, errors.New("--accept-self-signed and --plain-http ask for different " +
			"things; pass one of them")
	}

	ip, err := i.chooseIP()
	if err != nil {
		return site{}, err
	}

	printf(i.env.Stdout, "Installing to be reached at %s.\n", ip)
	if ip.IsPrivate() {
		printf(i.env.Stdout, "%s is a private address. If this host is behind NAT, the address\n", ip)
		printf(i.env.Stdout, "you actually reach it on is a different one -- pass that one with --ip,\n")
		printf(i.env.Stdout, "so that the certificate carries the name your browser will ask for.\n")
	}

	if !i.cfg.plainHTTP {
		want := i.cfg.selfSigned
		if !want {
			answered, err := i.confirm(selfSignedQuestion(ip), "Generate a self-signed certificate?")
			if err != nil {
				return site{}, err
			}
			want = answered
		}
		if want {
			certPEM, keyPEM, fingerprint, err := proxy.SelfSigned(ip, time.Now().Add(certLifetime))
			if err != nil {
				return site{}, fmt.Errorf("generating a certificate for %s: %w", ip, err)
			}
			return site{
				how:         proxy.AtIPWithTLS,
				ip:          ip,
				certPEM:     certPEM,
				keyPEM:      keyPEM,
				fingerprint: fingerprint,
				mode:        server.ModeIP,
				url: "https://" + net.JoinHostPort(ip.String(),
					strconv.Itoa(proxy.DefaultTLSPort)) + "/",
			}, nil
		}
	}

	return site{
		how:  proxy.AtIPPlain,
		ip:   ip,
		mode: server.ModeIP,
		url:  "http://" + ip.String() + "/",
	}, nil
}

func (i *installer) chooseIP() (net.IP, error) {
	if i.cfg.ip != "" {
		ip := net.ParseIP(i.cfg.ip)
		if ip == nil {
			return nil, fmt.Errorf("--ip %q is not an address", i.cfg.ip)
		}
		return ip, nil
	}
	ip, err := i.localIP()
	if err != nil {
		return nil, fmt.Errorf("working out this host's own address: %w; pass one with --ip", err)
	}
	return ip, nil
}

// selfSignedQuestion is the explanation the offer comes with. It says where
// dokkup will be reached, because 8443 is not what anyone would guess and the
// reason for it is not the operator's fault.
func selfSignedQuestion(ip net.IP) string {
	return fmt.Sprintf(
		"No certificate authority will vouch for an IP address, so dokkup can make\n"+
			"its own certificate. Your browser will warn about it once, and you can\n"+
			"check the fingerprint printed afterwards against the one it shows you.\n"+
			"It will be served at https://%s/ -- not on 443, which is owned by\n"+
			"Dokku's own catch-all and refuses connections that arrive without a name.\n\n"+
			"Answering no serves dokkup over plain HTTP at http://%s/ instead, where\n"+
			"anyone who can see the network can read everything you do in it.",
		net.JoinHostPort(ip.String(), strconv.Itoa(proxy.DefaultTLSPort)), ip)
}

// errNoAnswer reports that nothing was typed. It is separate from "no" so that
// each question can give its own advice about which flag to pass instead.
var errNoAnswer = errors.New("nothing was typed")

// yesOrNo puts a question and believes only a deliberate answer.
//
// There is no default. An empty line and a closed standard input are both
// [errNoAnswer] rather than "no": these questions precede changes to a host,
// and someone pressing return to get rid of a prompt has not chosen anything.
func yesOrNo(env Env, preamble, question string) (bool, error) {
	printf(env.Stdout, "\n%s\n\n%s [yes/no] ", preamble, question)

	line, err := readLine(env.Stdin)
	if err != nil {
		return false, fmt.Errorf("reading the answer: %w", err)
	}
	printf(env.Stdout, "\n")

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	case "":
		return false, errNoAnswer
	default:
		return false, fmt.Errorf("%q is not yes or no", strings.TrimSpace(line))
	}
}

// readLine reads one line and not a byte more.
//
// Unbuffered on purpose. Removal asks its own question and then hands the same
// standard input to [authn.Confirmer]; a bufio.Reader answering the first would
// have taken the answer to the second out of the pipe with it and left the
// challenge reading nothing. End of input is an empty line rather than an
// error, because a closed standard input has said nothing and that is the
// caller's to interpret.
func readLine(r io.Reader) (string, error) {
	var line []byte
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return string(line), nil
			}
			line = append(line, one[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return string(line), nil
			}
			return string(line), err
		}
	}
}

// confirm is [yesOrNo] with installation's own advice for a question nobody
// answered.
func (i *installer) confirm(preamble, question string) (bool, error) {
	yes, err := yesOrNo(i.env, preamble, question)
	if errors.Is(err, errNoAnswer) {
		return false, errors.New("there is no answer to read, and installation will not choose " +
			"for you: pass --accept-self-signed or --plain-http")
	}
	return yes, err
}

// mustConfirm is [installer.confirm] where "no" ends the installation.
func (i *installer) mustConfirm(preamble, question string) error {
	yes, err := i.confirm(preamble, question)
	if err != nil {
		return err
	}
	if !yes {
		return errors.New("stopped at your answer; nothing has been changed")
	}
	return nil
}

// ensureAccount creates the group, the user and the membership, and reports the
// resolved account so that what is created next can be given to it.
func (i *installer) ensureAccount(ctx context.Context) (host.Account, error) {
	hadGroup := true
	if _, err := i.ops.LookupGroup(ctx, hostpaths.Group); err != nil {
		if !errors.Is(err, host.ErrNoSuchGroup) {
			return host.Account{}, err
		}
		hadGroup = false
	}
	if err := i.ops.EnsureGroup(ctx, hostpaths.Group); err != nil {
		return host.Account{}, err
	}
	if !hadGroup {
		i.undoing("removing the group "+hostpaths.Group, func(ctx context.Context) error {
			_, err := i.ops.RemoveGroup(ctx, hostpaths.Group)
			return err
		})
	}

	created, err := i.ops.EnsureUser(ctx, host.UserSpec{
		Name:  hostpaths.User,
		Group: hostpaths.Group,
		Shell: host.Nologin,
		Home:  nonexistentHome,
	})
	if err != nil {
		return host.Account{}, err
	}
	if created {
		i.undoing("removing the user "+hostpaths.User, func(ctx context.Context) error {
			_, err := i.ops.RemoveUser(ctx, hostpaths.User)
			return err
		})
	}

	// Not undone, on purpose. userdel takes the membership with the user, so
	// there is nothing to undo for an account this run created; and for one it
	// found, removing a membership dokkup may not have added would be a change
	// to something it did not create (ADR-0008).
	//
	// What the membership buys is filesystem reach into DOKKU_ROOT, which is
	// 0750 dokku:dokku. It is not the right to run dokku -- that is the sudoers
	// rule below, on its own. The two are easy to conflate and are unrelated.
	if err := i.ops.JoinGroup(ctx, hostpaths.User, hostpaths.DokkuGroup); err != nil {
		return host.Account{}, err
	}

	account, err := i.ops.LookupUser(ctx, hostpaths.User)
	if err != nil {
		return host.Account{}, err
	}

	printf(i.env.Stdout, "The system user %s is in the %s group.\n", hostpaths.User, hostpaths.DokkuGroup)
	return account, nil
}

// ensureDataDir creates the state directory and the one the certificate lives
// in, and re-applies the mode and the owner every run, so that a directory an
// earlier installation got wrong is corrected rather than merely detected.
func (i *installer) ensureDataDir(ctx context.Context, account host.Account) error {
	for _, dir := range []string{hostpaths.DataDir, hostpaths.TLSDir} {
		path := i.path(dir)
		had := exists(path)

		if err := i.ops.EnsureDir(ctx, path, dataDirMode, account); err != nil {
			return err
		}
		if !had {
			i.undoing("removing "+dir, func(context.Context) error { return os.RemoveAll(path) })
		}
	}

	printf(i.env.Stdout, "%s is %#o, owned by %s.\n", hostpaths.DataDir, dataDirMode, hostpaths.User)
	return nil
}

// ensureSudoers writes the rule and then proves it, because a rule with the
// wrong mode, the wrong owner or the wrong runas is accepted without complaint
// and only fails later, from inside a service, as a health endpoint reporting
// that this host has no applications.
func (i *installer) ensureSudoers(ctx context.Context) error {
	path := i.path(hostpaths.Sudoers)
	if err := os.MkdirAll(filepath.Dir(path), systemDirMode); err != nil {
		return fmt.Errorf("creating the directory for %s: %w", hostpaths.Sudoers, err)
	}
	previous, had, err := readIfExists(path)
	if err != nil {
		return fmt.Errorf("reading the rule already at %s: %w", hostpaths.Sudoers, err)
	}

	// Two lines, and each names one program with its arguments.
	//
	// The first runs as the dokku user rather than root, so the ceiling on what
	// it can escalate to is that account rather than the whole host, and both
	// constants in it are the ones the service itself invokes through, so the
	// rule cannot come to permit something other than what is run.
	//
	// The second is the reload a renewed certificate needs, because nginx reads
	// certificates once, at load. It is already redundant on a stock Dokku host
	// -- Dokku's own /etc/sudoers.d/dokku-nginx grants the dokku group
	// `systemctl {enable,disable,reload,start,stop} nginx` and `nginx -t`, and
	// dokkup is in that group, measured with `sudo -l -U dokkup` -- and it is
	// written anyway, because a renewal that silently depends on the contents
	// of a file dokkup does not own would stop working on the day Dokku changes
	// it, ninety days before anyone found out.
	//
	// sudo matches arguments exactly, so this permits `systemctl reload nginx`
	// and no other verb and no other unit: measured, `systemctl reload ssh`
	// under this rule alone answers `sudo: a password is required`.
	rule := fmt.Sprintf("%s ALL=(%s) NOPASSWD: %s\n%s ALL=(root) NOPASSWD: %s reload %s\n",
		hostpaths.User, dokku.DefaultRunAs, dokku.DefaultBinary,
		hostpaths.User, service.DefaultBinary, nginxUnit)

	if err := i.ops.WriteSudoers(ctx, path, rule); err != nil {
		return err
	}
	i.undoing("putting back "+hostpaths.Sudoers, i.restorer(path, previous, had, 0o440))

	if err := i.ops.ProveSudo(ctx, hostpaths.User,
		dokku.DefaultRunAs, dokku.DefaultBinary, "version"); err != nil {
		return fmt.Errorf("the sudoers rule was written but does not work: %w", err)
	}

	printf(i.env.Stdout, "%s may run %s as %s, and nothing else.\n",
		hostpaths.User, dokku.DefaultBinary, dokku.DefaultRunAs)
	return nil
}

// installBinary puts this executable where the unit expects to find it.
//
// The staging file, the hardlink and the rename are [updater.swap]'s, for the
// same reasons, with one difference: there may be nothing to replace, and a
// first installation therefore keeps no previous version.
func (i *installer) installBinary() error {
	target := i.path(hostpaths.Binary)
	if sameFile(i.self, target) {
		printf(i.env.Stdout, "%s is already this executable; leaving it alone.\n", hostpaths.Binary)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), systemDirMode); err != nil {
		return fmt.Errorf("creating the directory for %s: %w", hostpaths.Binary, err)
	}

	staged := target + ".new"
	if err := copyFile(i.self, staged, binaryMode); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("copying this executable to %s: %w", staged, err)
	}

	previous := i.path(hostpaths.PreviousBinary)
	hadBinary := exists(target)
	if hadBinary {
		if err := os.Remove(previous); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(staged)
			return fmt.Errorf("clearing %s: %w", hostpaths.PreviousBinary, err)
		}
		if err := os.Link(target, previous); err != nil {
			_ = os.Remove(staged)
			return fmt.Errorf("keeping a copy of the binary being replaced: %w", err)
		}
	}

	if err := os.Rename(staged, target); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("installing to %s: %w", hostpaths.Binary, err)
	}

	if hadBinary {
		i.undoing("putting back the previous "+hostpaths.Binary, func(context.Context) error {
			return os.Rename(previous, target)
		})
	} else {
		i.undoing("removing "+hostpaths.Binary, func(context.Context) error {
			return os.Remove(target)
		})
	}

	printf(i.env.Stdout, "Installed %s.\n", hostpaths.Binary)
	return nil
}

// installCertificate writes what nginx will serve.
//
// The files are left owned by root inside a directory owned by dokkup at 0750.
// nginx reads them as root before it drops privileges, so nothing needs them to
// change hands; and because the directory is dokkup's, a renewal running as the
// service can still replace them by writing beside them and renaming, which is
// the only reason the paths are inside /var/lib rather than /etc.
func (i *installer) installCertificate(s site) error {
	if s.certPEM == nil {
		return nil
	}
	if err := i.replaceFile(i.path(hostpaths.TLSCert), s.certPEM, readableMode, "certificate"); err != nil {
		return err
	}
	if err := i.replaceFile(i.path(hostpaths.TLSKey), s.keyPEM, keyMode, "private key"); err != nil {
		return err
	}

	printf(i.env.Stdout, "The certificate is at %s.\n", hostpaths.TLSCert)
	return nil
}

// installVhost writes dokkup's one file under /etc/nginx, having proved twice
// that it is safe to.
//
// The candidate is tested outside /etc/nginx entirely first, because that is
// what catches a missing or unreadable certificate before anything enters the
// directory nginx globs. Then the file goes in by rename, and the whole live
// configuration is tested. A file that fails the second test is taken straight
// back out: nginx.service carries `ExecStartPre=/usr/sbin/nginx -t -q`, so
// leaving a broken file in conf.d does not break a reload -- it breaks the next
// start, which takes every app on this host down at the next reboot.
func (i *installer) installVhost(ctx context.Context, s site) error {
	text := proxy.VhostFile(proxy.VhostConfig{
		How:    s.how,
		Domain: s.domain,
		IP:     ipString(s.ip),
		Listen: i.cfg.listen,
	})

	candidate := filepath.Join(i.path(hostpaths.DataDir), "vhost.candidate")
	//nolint:gosec // nginx reads this as root and there is nothing secret in it
	if err := os.WriteFile(candidate, []byte(text), readableMode); err != nil {
		return fmt.Errorf("writing the vhost to test at %s: %w", candidate, err)
	}
	defer func() { _ = os.Remove(candidate) }()

	if err := i.ops.CheckProxyFragment(ctx, candidate); err != nil {
		return fmt.Errorf("the vhost dokkup would write does not pass nginx's own check, so "+
			"nothing was put into %s: %w", filepath.Dir(hostpaths.Vhost), err)
	}

	target := i.path(hostpaths.Vhost)
	previous, had, err := readIfExists(target)
	if err != nil {
		return fmt.Errorf("reading the vhost already at %s: %w", hostpaths.Vhost, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), systemDirMode); err != nil {
		return fmt.Errorf("creating the directory for %s: %w", hostpaths.Vhost, err)
	}

	// Staged under a name nginx.conf's `include conf.d/*.conf` cannot match, so
	// the file is never half-visible, and renamed within one directory, which
	// is atomic.
	staging := i.path(hostpaths.VhostStaging)
	//nolint:gosec // as above
	if err := os.WriteFile(staging, []byte(text), readableMode); err != nil {
		return fmt.Errorf("staging the vhost at %s: %w", hostpaths.VhostStaging, err)
	}
	if err := os.Rename(staging, target); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("putting the vhost at %s: %w", hostpaths.Vhost, err)
	}
	i.undoing("taking "+hostpaths.Vhost+" back out", i.vhostRestorer(target, previous, had))

	if err := i.ops.CheckProxy(ctx); err != nil {
		return fmt.Errorf("nginx refuses this host's configuration with dokkup's vhost in it: %w", err)
	}

	printf(i.env.Stdout, "%s is written and nginx accepts it.\n", hostpaths.Vhost)
	return nil
}

// vhostRestorer puts the vhost back and then proves the host is at its
// baseline, rather than assuming that undoing the change undid the problem.
func (i *installer) vhostRestorer(target string, previous []byte, had bool) func(context.Context) error {
	restore := i.restorer(target, previous, had, readableMode)
	return func(ctx context.Context) error {
		if err := restore(ctx); err != nil {
			return err
		}
		if err := i.ops.CheckProxy(ctx); err != nil {
			return fmt.Errorf("%s is back the way it was found, but nginx still refuses this "+
				"host's configuration, so this host needs attention: %w", hostpaths.Vhost, err)
		}
		return nil
	}
}

func (i *installer) installUnit(ctx context.Context, s site) error {
	unit := service.UnitFile(service.UnitConfig{
		Listen:            i.cfg.listen,
		Mode:              string(s.mode),
		Domain:            s.domain,
		ManageCertificate: s.manageCert,
		ACMEEmail:         i.cfg.acmeEmail,
		ACMEDirectory:     i.cfg.acmeDirectory,
	})

	restore, err := i.writeOwnedFile(i.path(hostpaths.Unit), []byte(unit), readableMode, "unit")
	if err != nil {
		return err
	}
	// The reload is part of undoing the file rather than an undo of its own, so
	// that unwinding restores the text and then tells systemd about it, in that
	// order. Two separate steps would run in the opposite one.
	i.undoing("putting back "+hostpaths.Unit, func(ctx context.Context) error {
		if err := restore(ctx); err != nil {
			return err
		}
		return i.manager.DaemonReload(ctx)
	})

	if err := i.manager.DaemonReload(ctx); err != nil {
		return fmt.Errorf("telling systemd to read %s: %w", hostpaths.Unit, err)
	}
	return nil
}

// startService brings the unit up, and brings it up again if it was already.
//
// `systemctl enable --now` starts a stopped unit and leaves a running one
// exactly as it was, so on a host that already has dokkup on it this alone
// would leave the previous process serving -- the previous binary, the previous
// domain, the previous flags -- while every check in this file passed, because
// the health endpoint answers with a version that has not changed either.
// Measured on the dev environment: re-installing with --manage-certificate left
// a service running without it, and the certificate never came.
func (i *installer) startService(ctx context.Context) error {
	running, err := i.manager.IsActive(ctx)
	if err != nil {
		return fmt.Errorf("asking whether %s is already running: %w", service.Name, err)
	}

	if err := i.manager.Enable(ctx); err != nil {
		return fmt.Errorf("enabling %s: %w", service.Name, err)
	}
	i.undoing("stopping and disabling "+service.Name, i.manager.Disable)

	if running {
		if err := i.manager.Restart(ctx); err != nil {
			return fmt.Errorf("restarting %s onto what was just installed: %w", service.Name, err)
		}
	}

	printf(i.env.Stdout, "Waiting for %s to answer...\n", service.Name)
	return waitHealthy(ctx, i.health, i.version, i.timeout, i.interval)
}

// reloadProxy asks nginx to read the new file.
func (i *installer) reloadProxy(ctx context.Context) error {
	if err := i.manager.Reload(ctx, nginxUnit); err != nil {
		return fmt.Errorf("asking nginx to read %s: %w", hostpaths.Vhost, err)
	}
	return nil
}

// confirmSite goes and looks, rather than believing the reload's exit status.
//
// Looking is not belt and braces. `systemctl reload nginx` returns before the
// new configuration is answering, so a reload reported on its exit status is
// how flaky installations are made. It is also the only check that catches a
// Dokku app which already claims dokkup's domain: dokku.conf sorts before
// dokkup.conf, nginx emits a warning rather than an error, `nginx -t` still
// exits 0, and the app wins silently.
func (i *installer) confirmSite(ctx context.Context, s site) error {
	if err := i.confirmProxy(ctx, s); err != nil {
		if s.how == proxy.AtDomain {
			return fmt.Errorf("dokkup is running, but %s does not reach it through nginx; a "+
				"Dokku app that already claims this name would win silently, because its vhost "+
				"is read first and nginx only warns about it: %w", s.url, err)
		}
		return fmt.Errorf("dokkup is running, but %s does not reach it through nginx: %w", s.url, err)
	}

	printf(i.env.Stdout, "nginx is serving dokkup at %s.\n", s.url)
	return nil
}

// unwind undoes what this run changed, newest first.
//
// An undo that fails is reported and does not stop the rest: a host is better
// off with four of five things undone than with one. The context is detached
// from the one that may have just been cancelled, because an unwind that could
// not run would leave exactly the half-changed host this exists to prevent.
func (i *installer) unwind(ctx context.Context) {
	if len(i.undo) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)

	printf(i.env.Stderr, "\nPutting this host back the way it was found:\n")
	for k := len(i.undo) - 1; k >= 0; k-- {
		step := i.undo[k]
		printf(i.env.Stderr, "  %s\n", step.what)
		if err := step.run(ctx); err != nil {
			printf(i.env.Stderr, "    could not: %v\n", err)
		}
	}
	printf(i.env.Stderr, "\n")
}

// report says what is running and where to find it.
func (i *installer) report(s site) {
	printf(i.env.Stdout, "\ndokkup %s is installed and answering.\n\n", i.version)
	printf(i.env.Stdout, "  Reach it at %s\n\n", s.url)

	if s.fingerprint != "" {
		printf(i.env.Stdout, "  Your browser will warn that this certificate is not trusted,\n")
		printf(i.env.Stdout, "  because no certificate authority can vouch for an address. Check\n")
		printf(i.env.Stdout, "  that it shows this fingerprint before going past the warning:\n\n")
		printf(i.env.Stdout, "    %s\n\n", s.fingerprint)
	}
	if s.manageCert {
		printf(i.env.Stdout, "  dokkup holds this certificate itself and renews it thirty days before\n")
		printf(i.env.Stdout, "  it expires, so nothing here is on a calendar. Renewal needs %s\n", i.cfg.domain)
		printf(i.env.Stdout, "  to keep reaching this host on port 80; a certificate authority checks\n")
		printf(i.env.Stdout, "  that every time, not only the first.\n\n")
	}
	if s.how == proxy.AtIPPlain {
		printf(i.env.Stdout, "  This is plain HTTP. Anyone who can see the network between you and\n")
		printf(i.env.Stdout, "  this host can read everything you do in dokkup, including the token\n")
		printf(i.env.Stdout, "  that creates the owner. Publish at a domain when you have one.\n\n")
	}
	if s.mode == server.ModeIP {
		printf(i.env.Stdout, "  dokkup is in IP mode, so it allows only the owner and says so on\n")
		printf(i.env.Stdout, "  every screen.\n\n")
	}

	printf(i.env.Stdout, "  Creating the owner account needs a single-use token, which this\n")
	printf(i.env.Stdout, "  version does not issue yet: when it does, the installer prints one\n")
	printf(i.env.Stdout, "  here and 'dokkup setup-token' reissues it while no owner exists.\n\n")
}

// writeOwnedFile writes one of dokkup's files and returns how to put back
// whatever was there, without recording it: the caller decides when, and
// whether anything else has to happen in the same step.
func (i *installer) writeOwnedFile(path string, content []byte, mode fs.FileMode,
	what string) (func(context.Context) error, error) {
	if err := os.MkdirAll(filepath.Dir(path), systemDirMode); err != nil {
		return nil, fmt.Errorf("creating the directory for %s: %w", path, err)
	}
	previous, had, err := readIfExists(path)
	if err != nil {
		return nil, fmt.Errorf("reading the %s already at %s: %w", what, path, err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}
	// WriteFile's mode is filtered by the umask and is ignored outright for a
	// file that already exists, so neither a re-run nor a first run gets what it
	// asked for without this.
	if err := os.Chmod(path, mode); err != nil {
		return nil, fmt.Errorf("setting the mode of %s: %w", path, err)
	}
	return i.restorer(path, previous, had, mode), nil
}

// replaceFile is [installer.writeOwnedFile] for the files whose undo is nothing
// but putting them back.
func (i *installer) replaceFile(path string, content []byte, mode fs.FileMode, what string) error {
	restore, err := i.writeOwnedFile(path, content, mode, what)
	if err != nil {
		return err
	}
	i.undoing("putting back "+path, restore)
	return nil
}

// restorer returns an undo that leaves path the way this run found it, which
// for a file that was not there is not there.
func (i *installer) restorer(path string, previous []byte, had bool,
	mode fs.FileMode) func(context.Context) error {
	return func(context.Context) error {
		if !had {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		}
		if err := os.WriteFile(path, previous, mode); err != nil {
			return err
		}
		return os.Chmod(path, mode)
	}
}

// systemDirMode is what a directory dokkup writes into is created as, on the
// vanishingly rare host that does not have it already.
//
// /usr/local/bin, /etc/systemd/system, /etc/sudoers.d and /etc/nginx/conf.d all
// exist on any host that has Dokku on it, so on a real installation every
// MkdirAll here does nothing at all; what they are for is a test whose root is
// a t.TempDir(). 0755 rather than something tighter because these are the
// modes those directories actually have, and a traversable /usr/local/bin is
// not optional.
const systemDirMode = 0o755

// exists reports whether there is anything at path.
//
// A path that cannot be stat'ed for some other reason is reported as absent,
// which is the answer every caller here wants: they are all deciding whether
// there is something to create, to remove or to ask about, and the operation
// itself is what reports the real error.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readIfExists(path string) ([]byte, bool, error) {
	content, err := os.ReadFile(path) //nolint:gosec // one of dokkup's own paths
	switch {
	case err == nil:
		return content, true, nil
	case errors.Is(err, os.ErrNotExist):
		return nil, false, nil
	default:
		return nil, false, err
	}
}

// sameFile reports whether two paths are the same file, which is how
// installation notices it is being run from the place it installs to -- the
// route the README documents, `install -m 0755 dokkup_linux_amd64
// /usr/local/bin/dokkup && sudo dokkup install`.
func sameFile(a, b string) bool {
	infoA, err := os.Stat(a)
	if err != nil {
		return false
	}
	infoB, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(infoA, infoB)
}

func copyFile(from, to string, mode fs.FileMode) error {
	src, err := os.Open(from) //nolint:gosec // the executable that is running
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) //nolint:gosec // beside hostpaths.Binary, which is dokkup's own

	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	// Flushed before the rename that follows, so that a host losing power
	// between the two finds either the old binary or the whole new one.
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Chmod(to, mode)
}

func ipString(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}

func joinIPs(ips []net.IP) string {
	parts := make([]string, 0, len(ips))
	for _, ip := range ips {
		parts = append(parts, ip.String())
	}
	return strings.Join(parts, ", ")
}

// resolveDomain is the lookup the domain check uses on a real host.
func resolveDomain(ctx context.Context, name string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	return net.DefaultResolver.LookupIP(ctx, "ip", name)
}

// localAddresses reports every address this host holds, which is more than
// [proxy.LocalIP] can: that one answers with the address the default route
// would use, and a host reached on a second interface has others.
func localAddresses() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if network, ok := addr.(*net.IPNet); ok {
			ips = append(ips, network.IP)
		}
	}
	return ips, nil
}

// proxyProbe builds the client and URL that ask what nginx serves.
//
// The connection is made to the loopback address whatever the URL says, while
// the URL's host is what reaches nginx -- as the SNI name, as the Host header,
// or both. That is deliberate: what is being asked is which server block
// answers for this name, and nginx decides that from the name rather than from
// the address the connection arrived on. Dialling the public address instead
// would fail on every host behind NAT and would prove nothing extra on the rest.
func proxyProbe(s site) (*http.Client, string) {
	name, port, scheme := s.domain, "443", "https"
	switch s.how {
	case proxy.AtIPWithTLS:
		name, port = s.ip.String(), strconv.Itoa(proxy.DefaultTLSPort)
	case proxy.AtIPPlain:
		name, port, scheme = s.ip.String(), "80", "http"
	case proxy.AtDomain:
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		},
	}
	if scheme == "https" {
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			// The chain is not what is being asked about here. For a
			// self-signed certificate the fingerprint below is a stronger check
			// than a trust store could make, and for a supplied one the
			// certificate was already read, paired and checked against the
			// domain before it was installed. What is left to find out is
			// whether nginx is serving dokkup, and a verification failure would
			// answer that question with the wrong error.
			InsecureSkipVerify: true, //nolint:gosec // see above; the fingerprint is checked instead
			VerifyConnection:   verifyFingerprint(s.fingerprint),
		}
	}

	client := &http.Client{Timeout: healthProbeTimeout, Transport: transport}
	return client, scheme + "://" + net.JoinHostPort(name, port) + "/api/health"
}

// verifyFingerprint checks that what is being served is the certificate this
// run installed, which is what makes the fingerprint printed for the operator
// true rather than merely intended.
//
// It hangs off VerifyConnection rather than VerifyPeerCertificate because a
// resumed session skips the latter: the check would pass by not running.
func verifyFingerprint(want string) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		if want == "" {
			return nil
		}
		if len(state.PeerCertificates) == 0 {
			return errors.New("no certificate was served at all")
		}
		if got := proxy.Fingerprint(state.PeerCertificates[0].Raw); got != want {
			return fmt.Errorf("the certificate being served is %s, not the %s just installed", got, want)
		}
		return nil
	}
}

// printInstallPlan states what installation will do. It is printed before
// anything happens, and it names the same locations the uninstaller reads from
// hostpaths, so the two cannot drift apart.
//
// The two ways of installing at a domain differ in what they will do to this
// host -- one serves a file, the other talks to a certificate authority on the
// operator's behalf and goes on doing so every sixty days -- so the plan says
// which, and names the authority it will be talking to.
func printInstallPlan(env Env, cfg installConfig) {
	domain := cfg.domain
	ownCertificate := cfg.cert != "" || cfg.key != ""
	printf(env.Stdout, "dokkup install will:\n\n")

	printf(env.Stdout, "  Create the system user %q in the %q group, with a sudoers rule\n",
		hostpaths.User, hostpaths.DokkuGroup)
	printf(env.Stdout, "  permitting exactly one program without a password: the dokku binary.\n\n")

	printf(env.Stdout, "  Write:\n")
	for _, path := range hostpaths.Owned() {
		printf(env.Stdout, "    %s\n", path)
	}
	printf(env.Stdout, "\n")

	printf(env.Stdout, "  Leave this host's Dokku plugins alone, including a Let's Encrypt plugin\n")
	printf(env.Stdout, "  if it has one: other apps depend on it for certificate renewal, and it\n")
	printf(env.Stdout, "  issues certificates for apps rather than for dokkup.\n\n")

	switch {
	case domain != "" && ownCertificate:
		printf(env.Stdout, "  Serve dokkup at %s with the certificate you supply with --cert and\n", domain)
		printf(env.Stdout, "  --key, and renew nothing: that certificate is yours to replace. If the\n")
		printf(env.Stdout, "  name does not resolve to this host you will be told, and asked whether\n")
		printf(env.Stdout, "  to continue.\n\n")
	case domain != "":
		printf(env.Stdout, "  Serve dokkup at %s, and get a certificate for it from %s.\n",
			domain, certificateAuthority(cfg.acmeDirectory))
		printf(env.Stdout, "  That means agreeing to their subscriber agreement on your behalf, and\n")
		printf(env.Stdout, "  it means %s must reach this host on port 80 from the internet:\n", domain)
		printf(env.Stdout, "  that is how a certificate authority proves the name is yours. dokkup\n")
		printf(env.Stdout, "  serves a self-signed certificate until the real one arrives, and\n")
		printf(env.Stdout, "  renews from then on, thirty days before expiry. Pass --cert and --key\n")
		printf(env.Stdout, "  instead to serve one you already have.\n\n")
	default:
		printf(env.Stdout, "  Serve dokkup at this host's IP address. No certificate authority will\n")
		printf(env.Stdout, "  vouch for an address, so you will be offered a self-signed certificate\n")
		printf(env.Stdout, "  and shown its fingerprint to check in the browser. It is served on\n")
		printf(env.Stdout, "  port %d, because port 443 is owned by Dokku's own catch-all. In this\n",
			proxy.DefaultTLSPort)
		printf(env.Stdout, "  mode dokkup allows only the owner and warns on every screen. Leave it\n")
		printf(env.Stdout, "  later with 'dokkup publish <domain>'.\n\n")
	}

	printf(env.Stdout, "  Print the address to reach dokkup at. Creating the owner account needs\n")
	printf(env.Stdout, "  a single-use token, which this version does not issue yet: when it does,\n")
	printf(env.Stdout, "  the installer prints one here and 'dokkup setup-token' reissues it while\n")
	printf(env.Stdout, "  no owner exists.\n\n")
}

// certificateAuthority names who will be issuing, for a plan that has to be
// true of the run it is describing. An operator who pointed --acme-directory
// somewhere else must not be told this host is about to talk to Let's Encrypt.
func certificateAuthority(directory string) string {
	if directory == "" {
		return "Let's Encrypt"
	}
	return directory
}

func runPublish(env Env, args []string) error {
	fs := newFlagSet(env, "publish")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: dokkup publish <domain>")
	}

	domain := fs.Arg(0)
	printf(env.Stdout, "dokkup publish will check that %s resolves to this host, serve dokkup\n", domain)
	printf(env.Stdout, "there with a certificate, and leave IP mode.\n\n")

	return fmt.Errorf("%w: publishing is not built yet", ErrNotImplemented)
}

func runSetupToken(env Env, args []string) error {
	fs := newFlagSet(env, "setup-token")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	printf(env.Stdout, "dokkup setup-token issues a single-use token for creating the owner.\n")
	printf(env.Stdout, "It refuses once an owner exists: without that condition it would be an\n")
	printf(env.Stdout, "unauthenticated route to taking over the account.\n\n")

	return fmt.Errorf("%w: token issuance is not built yet", ErrNotImplemented)
}
