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
}

// New builds a server from cfg.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeIP
	}

	s := &Server{cfg: cfg, mux: http.NewServeMux(), logger: cfg.Logger}
	s.routes()
	return s
}

// Handler returns the root handler, with the middleware chain applied.
func (s *Server) Handler() http.Handler {
	return s.withSecurityHeaders(s.withRequestLogging(s.mux))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/session", s.handleSession)

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
// mode dokkup is in, and therefore whether to show the warning banner.
func (s *Server) handleSession(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":           string(s.cfg.Mode),
		"ownerOnly":      s.cfg.Mode.Restricted(),
		"authenticated":  false,
		"setupCompleted": false,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("writing response", "error", err)
	}
}
