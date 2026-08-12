// Package server serves dokkup's HTTP API and its embedded frontend.
//
// There is no server-side rendering, so there is no server-side route guard:
// the frontend's guards are a user-experience affordance and authorisation is
// enforced here or not at all. See
// docs/adr/0004-single-go-binary-with-embedded-csr-frontend.md.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/dokku"
	"github.com/eduardotorresdev/dokkup/internal/secret"
	"github.com/eduardotorresdev/dokkup/internal/store"
)

// Mode is how dokkup is reachable, which decides what it will allow.
type Mode string

const (
	// ModePublished means dokkup is served at a domain with a certificate a
	// browser trusts. The full feature set is available.
	ModePublished Mode = "published"

	// ModeIP means dokkup is reached by IP address, where no certificate
	// authority will vouch for it. dokkup restricts itself to the owner and
	// says so on every screen. See docs/adr/0006-publishing-tls-and-ip-mode.md.
	ModeIP Mode = "ip"
)

// Restricted reports whether the mode forbids more than one operator.
func (m Mode) Restricted() bool { return m == ModeIP }

// Config is what the server needs to run.
type Config struct {
	// Dokku is the seam through which Dokku is invoked.
	Dokku dokku.Client

	// Mode decides whether additional operators may be created.
	Mode Mode

	// PlainHTTP tells the server that it is reached over plain HTTP, so the
	// session cookie must not carry Secure -- a browser drops a Secure cookie
	// on an insecure origin, and a dropped cookie is a sign-in that silently
	// fails. Default false, which is the fail-closed direction.
	PlainHTTP bool

	// Store is dokkup's Own State: the operators, and the Setup Token that
	// creates the first of them.
	//
	// It may be nil, and a nil store is not the same as a broken one. A dokkup
	// started without a database can still report its version and whether Dokku
	// answers -- which is exactly what an operator diagnosing a host that will
	// not start needs -- so /api/health and /api/session go on working, and the
	// routes that need state say so with a 503 rather than the process refusing
	// to run. `dokkup serve` never passes nil: it opens the database or exits,
	// because a server that cannot authenticate anybody is not serving.
	Store *store.Store

	// Version is the running binary's own version, reported by /api/health.
	//
	// It is what makes the endpoint usable as a gate after a restart: without
	// it, a service that came back on the *old* binary answers exactly as
	// happily as one that came back on the new one, and `dokkup update` would
	// call a failed update a success.
	Version string

	// Logger receives request and error logs.
	Logger *slog.Logger
}

// Server serves the API and the embedded frontend.
type Server struct {
	cfg    Config
	mux    *http.ServeMux
	logger *slog.Logger

	// setupLimiter is what stands between an unauthenticated stranger and
	// unlimited attempts at the Setup Token. It belongs to the server rather
	// than to the handler so that it survives requests, and it is a field
	// rather than a package variable so that two servers in one test process do
	// not share a budget.
	setupLimiter *tokenBucket

	// signinLimiter is the same guard over sign-in, and it is a second bucket
	// rather than the one above deliberately. Sharing would mean a stranger
	// hammering /api/setup -- a route that refuses everything once the Owner
	// exists, and so costs them nothing -- could spend the budget the Owner
	// needs to sign in with. A rate limit that can be exhausted on somebody
	// else's behalf is a denial of service with a documented interface.
	signinLimiter *tokenBucket

	// verifyPassword is [secret.VerifyPassword], reached through a field for
	// one reason: the sign-in path has to pay for a verification even when the
	// address it was given belongs to nobody -- see [dummyPasswordHash] -- and
	// a call whose only effect is how long it takes is a call nothing can
	// assert on. Deleting it leaves every test green while reopening the timing
	// channel that tells an attacker which addresses are operators here. With
	// the field, a test can watch the call happen.
	//
	// It is not a seam for anything else. Nothing outside this package can set
	// it, and there is no configuration that changes how a password is checked.
	verifyPassword func(encoded, password string) (bool, error)
}

// New builds a server from cfg.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeIP
	}

	s := &Server{
		cfg:    cfg,
		mux:    http.NewServeMux(),
		logger: cfg.Logger,
		// The real clock. Tests in this package replace the whole bucket with
		// one reading a clock they control, which is the only way to assert a
		// per-minute limit without waiting a minute.
		setupLimiter:   newTokenBucket(setupBurst, setupRefill, time.Now),
		signinLimiter:  newTokenBucket(signinBurst, signinRefill, time.Now),
		verifyPassword: secret.VerifyPassword,
	}
	s.routes()
	return s
}

// Handler returns the root handler, with the middleware chain applied.
//
// Outermost first: the headers hold for every response including the ones the
// guards write; logging then sees the status those guards produced; CSRF runs
// before authentication so that a forged cross-site request is refused without
// a database read; and the session check runs last, immediately before the mux,
// so that every route in it is behind one guard rather than each remembering
// its own. See [apiOnly] for why the two inner ones stop at /api/.
func (s *Server) Handler() http.Handler {
	guarded := apiOnly(requireCSRF)(apiOnly(s.requireSession)(s.mux))
	return s.withSecurityHeaders(s.withRequestLogging(guarded))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/session", s.handleSession)

	// Each mutation registers two patterns: the one with the method carries the
	// handler, and the bare path catches every other method so that a GET here
	// answers 405 instead of falling through to the catch-all below and being
	// served the application shell with a 200 -- which is what a client with a
	// bug reads as success.
	//
	// Neither wears a guard of its own any more. CSRF and the session check are
	// applied to the whole prefix in [Server.Handler], so the thing to check
	// when reading this table is the allowlist in unauthenticatedRoutes, not
	// each line.
	s.mux.HandleFunc("POST /api/setup", s.handleSetup)
	s.mux.HandleFunc("/api/setup", methodNotAllowed("the setup token is redeemed with POST"))

	s.mux.HandleFunc("POST /api/signin", s.handleSignIn)
	s.mux.HandleFunc("/api/signin", methodNotAllowed("signing in is done with POST"))

	s.mux.HandleFunc("POST /api/signout", s.handleSignOut)
	s.mux.HandleFunc("/api/signout", methodNotAllowed("signing out is done with POST"))

	// Everything not under /api is the single-page application: unknown paths
	// are routes it owns, so they must return the shell rather than a 404.
	s.mux.Handle("/", staticHandler())
}

// HealthDokkuTimeout bounds the one Dokku call the health endpoint makes.
//
// It is short on purpose, and it is the shorter half of a pair. The caller that
// matters most is `dokkup update`, which polls this endpoint to decide whether a
// newly installed binary came up -- and the answer it needs, the running
// dokkup's own version, is already in hand before Dokku is asked at all. If a
// wedged Docker could hold the reply past the updater's patience, the updater
// would see no answer, conclude the new binary never started, and roll back one
// that was working perfectly. So this must stay comfortably below the timeout on
// the probe client in internal/cli/update.go; the two are documented against
// each other, and a test in that package asserts the gap.
const HealthDokkuTimeout = 2 * time.Second

// handleHealth reports which dokkup is running and whether it can reach Dokku.
// It is deliberately unauthenticated: it exposes no state beyond reachability
// and a version, and an operator diagnosing a broken install needs it before
// being able to sign in.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), HealthDokkuTimeout)
	defer cancel()

	// Reported even when Dokku is unreachable, because that is exactly when an
	// updater needs to know which binary answered.
	body := map[string]any{"status": "ok", "dokkup": s.cfg.Version}
	status := http.StatusOK

	if s.cfg.Dokku != nil {
		version, err := s.cfg.Dokku.Version(ctx)
		if err != nil {
			s.logger.Error("dokku unreachable", "error", err)
			body["status"] = "degraded"
			body["dokku"] = "unreachable"
			status = http.StatusServiceUnavailable
		} else {
			body["dokku"] = version
		}
	}

	writeJSON(w, status, body)
}

// handleSession reports what the client needs before rendering anything: which
// mode dokkup is in, and therefore whether to show the warning banner, and
// whether this installation has been set up at all.
//
// It answers whether an Owner exists to a caller who has proved nothing, and
// that is deliberate: it is one bit, the frontend cannot decide between the
// setup screen and the sign-in screen without it, and the alternative -- making
// the client discover it by attempting a redemption -- would mean handing every
// stranger a rate-limited guess instead of an answer.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	// Whether it was possible to ask, as opposed to what the answer was. A
	// database that cannot be read must not report an installation as
	// unclaimed, because "no owner yet" is the state in which the setup screen
	// invites somebody to become one.
	setupCompleted := false
	if s.cfg.Store != nil {
		owned, err := s.cfg.Store.HasOwner(r.Context())
		if err != nil {
			// Logged rather than returned: this endpoint is what the frontend
			// loads before anything else, and failing it turns a database
			// hiccup into a blank page. The claim it makes is the safe one --
			// see below -- and /api/setup answers honestly either way.
			s.logger.Error("reading whether this installation has an owner", "error", err)
			// Claimed, so that the client offers to sign in rather than to take
			// the host over. A redemption attempted anyway is refused by the
			// store, which is the authority; this is only what the screen shows.
			owned = true
		}
		setupCompleted = owned
	}

	body := map[string]any{
		"mode":      string(s.cfg.Mode),
		"ownerOnly": s.cfg.Mode.Restricted(),
		// The session was resolved by [Server.requireSession], which runs over
		// the allowlisted routes as well precisely so that this one does not
		// read the cookie for itself.
		"authenticated":  false,
		"setupCompleted": setupCompleted,
	}

	// Present only when there is somebody to describe. An operator field that is
	// always there and sometimes null is a field every caller has to test twice.
	if operator, ok := operatorFrom(r.Context()); ok {
		body["authenticated"] = true
		body["operator"] = operatorBody(operator)
	}

	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("writing response", "error", err)
	}
}
