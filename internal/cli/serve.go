package cli

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/acme"
	"github.com/eduardotorresdev/dokkup/internal/dokku"
	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/server"
	"github.com/eduardotorresdev/dokkup/internal/service"
	"github.com/eduardotorresdev/dokkup/internal/store"
)

// reloadProxyAsService asks nginx to read a certificate that has just been
// renewed, from the unprivileged account the service runs as.
//
// It goes through sudo because the service is not root. What permits it is one
// line in dokkup's own sudoers file. That line is deliberately written even
// though it is already redundant on a stock Dokku host: Dokku installs
// /etc/sudoers.d/dokku-nginx granting the dokku group `systemctl {enable,
// disable, reload, start, stop} nginx` and `nginx -t`, and dokkup joins that
// group -- measured with `sudo -l -U dokkup` on the devenv. Relying on it would
// make dokkup's renewals depend on the contents of a file dokkup does not own
// and did not write.
func reloadProxyAsService() func(ctx context.Context) error {
	systemd := service.NewSystemctl()
	systemd.Sudo = dokku.DefaultSudo

	return func(ctx context.Context) error { return systemd.Reload(ctx, nginxUnit) }
}

// errFlagHelp is returned when a command was asked for its own usage, so that Run
// can exit zero rather than treating it as a failure.
var errFlagHelp = errors.New("help requested")

func newFlagSet(env Env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet("dokkup "+name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errFlagHelp
		}
		return err
	}
	return nil
}

func runServe(env Env, args []string) error {
	fs := newFlagSet(env, "serve")
	listen := fs.String("listen", service.DefaultListen, "address to listen on")
	mode := fs.String("mode", string(server.ModeIP), "reachability mode: published or ip")
	dokkuBinary := fs.String("dokku-binary", dokku.DefaultBinary, "path to the dokku executable")
	dokkuRunAs := fs.String("dokku-run-as", dokku.DefaultRunAs,
		"account to run dokku as, through sudo; empty invokes it directly")
	domain := fs.String("domain", "", "the name dokkup answers to, when it has one")
	manageCert := fs.Bool("manage-certificate", false,
		"obtain and renew the certificate for --domain over ACME")
	acmeEmail := fs.String("acme-email", "",
		"contact address the certificate authority warns about expiry at")
	acmeDirectory := fs.String("acme-directory", "",
		"ACME directory URL; empty means Let's Encrypt")
	tlsDir := fs.String("tls-dir", hostpaths.TLSDir, "where the certificate and its keys live")
	// The server cannot work this out for itself: nginx terminates TLS in front
	// of it, so what arrives is a plain HTTP request whether or not the browser
	// spoke HTTPS. The flag exists because the session cookie's Secure flag
	// depends on the answer, and it is off by default so that an installation
	// which forgot to say anything gets the protected cookie.
	plainHTTP := fs.Bool("plain-http", false,
		"this host is reached over plain HTTP, so the session cookie must not be Secure")
	// The default is the one place a dokkup service ever keeps its state, and
	// the flag exists for the two cases that are not a service: `make dev` on a
	// laptop that has no /var/lib/dokkup, and a second instance pointed at a
	// copy of a database while debugging one.
	dbPath := fs.String("db", hostpaths.DB, "the SQLite file holding dokkup's own state")

	if err := parseFlags(fs, args); err != nil {
		return err
	}

	if *manageCert && *domain == "" {
		return errors.New("--manage-certificate needs a --domain to get a certificate for")
	}

	logger := slog.Default()

	// Built through [dokku.NewExecClient] rather than as a literal, so the
	// service reaches Dokku the one way that works for the account it runs as:
	// `sudo -n -u dokku`, which is what the sudoers rule installation writes
	// permits. Invoking the binary directly fails for the service user with
	// `sudo: sorry, you are not allowed to preserve the environment`; the
	// measurement is on [dokku.ExecClient.RunAs].
	//
	// --dokku-run-as exists because that is a statement about how this host is
	// set up rather than a fact about dokkup, and an operator on an unusual one
	// -- Dokku somewhere else, a wrapper of their own, a rule written by hand
	// -- can turn the hop off with --dokku-run-as="" without patching anything.
	client := dokku.NewExecClient()
	client.Binary = *dokkuBinary
	client.RunAs = *dokkuRunAs

	// Established before anything is served, and before the listener exists, so
	// that a signal arriving during a migration stops it rather than being
	// swallowed by a startup that had not begun listening yet.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Opening the database is what runs the migrations, so this is also where
	// a schema change lands. Failing here is fatal on purpose: everything an
	// operator can do with dokkup begins with signing in, and a server that
	// cannot read its operators would come up looking healthy and refuse every
	// single person -- including, once the Setup Token is redeemed, the one who
	// owns the host. A running dokkup that nobody can use is worse than one
	// that says why it did not start.
	db, err := store.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	// Closing is what checkpoints the write-ahead log back into the one file a
	// backup is a copy of, so it runs on every way out of this function and not
	// only on the graceful one.
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("closing the database", "error", err)
		}
	}()

	srv := server.New(server.Config{
		Dokku:     client,
		Mode:      server.Mode(*mode),
		PlainHTTP: *plainHTTP,
		Store:     db,
		Version:   env.Build.Version,
		Logger:    logger,
	})

	handler := srv.Handler()

	if *manageCert {
		certificates := &acme.Manager{
			Domain:       *domain,
			Email:        *acmeEmail,
			DirectoryURL: *acmeDirectory,
			Dir:          *tlsDir,
			Reload:       reloadProxyAsService(),
			Logger:       logger,
		}

		// The challenge is answered ahead of everything else, including any
		// authentication the interface grows: RFC 8555 fixes the path, and a
		// certificate authority arrives as an anonymous stranger by definition.
		// Serving one token it asked for is the whole of what is exposed --
		// [acme.Manager.Handler] 404s anything not outstanding.
		mux := http.NewServeMux()
		mux.Handle(acme.ChallengePath, certificates.Handler())
		mux.Handle("/", handler)
		handler = mux

		go certificates.Run(ctx)
	}

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		logger.Info("dokkup listening", "address", *listen, "mode", *mode)
		printf(env.Stdout, "dokkup listening on http://%s\n", *listen)
		errc <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}
