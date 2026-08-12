package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/secret"
	"github.com/eduardotorresdev/dokkup/internal/store"
)

// apiPrefix is the surface the two guards below cover. Everything outside it is
// the single-page application, which is served to anybody: a browser that
// cannot fetch the shell cannot render the sign-in screen either, and there is
// nothing in it that is not already in the binary anyone can download.
const apiPrefix = "/api/"

// csrfHeader must be present on every state-changing request.
//
// The frontend is client-rendered, so there are no SvelteKit form actions to
// piggyback protection on. A browser will not attach a custom header to a
// cross-origin request without a successful preflight, which makes requiring one
// an effective second factor alongside the SameSite session cookie.
const csrfHeader = "X-Dokkup-CSRF"

// withSecurityHeaders applies the response headers that hold for every route.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		// The frontend is entirely self-hosted and loads nothing from anywhere
		// else, so the policy can be strict without qualification.
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"img-src 'self' data:; "+
				"style-src 'self' 'unsafe-inline'; "+
				"connect-src 'self'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'none'; "+
				"form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")

		next.ServeHTTP(w, r)
	})
}

// apiOnly narrows a guard to [apiPrefix], leaving everything else to pass
// through untouched.
//
// It exists so that [requireCSRF] and [Server.requireSession] can be written as
// ordinary middleware, tested as ordinary middleware, and still not apply to
// the catch-all that serves the frontend. The alternative -- teaching each
// guard to check the path itself -- puts the same condition in two places and
// makes each one's test assert routing as well as the rule it is about.
func apiOnly(guard func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		guarded := guard(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, apiPrefix) {
				next.ServeHTTP(w, r)
				return
			}
			guarded.ServeHTTP(w, r)
		})
	}
}

// unauthenticatedRoutes is every path under [apiPrefix] that may be reached
// without a session, and it is an allowlist rather than a blocklist on purpose.
//
// A route added to the mux tomorrow is protected by default. Written the other
// way round -- a list of paths that need a session -- the mistake of forgetting
// to add one is silent, and the thing it silently does is publish whatever the
// new route reads. There is no server-side rendering here, so this is where
// authorisation happens or it does not happen at all (ADR-0004).
//
// Each entry earns its place:
//
//   - /api/health exposes a version and whether Dokku answers, and an operator
//     diagnosing a host that will not start needs it before they can sign in.
//   - /api/session is what the frontend loads before it can decide between the
//     setup screen and the sign-in screen.
//   - /api/setup is the one unauthenticated mutation dokkup has, and ADR-0007
//     is why: without it the Owner could never be created.
//   - /api/signin is how a session comes to exist.
var unauthenticatedRoutes = map[string]bool{
	"/api/health":  true,
	"/api/session": true,
	"/api/setup":   true,
	"/api/signin":  true,
}

// requireSession refuses every API request that did not arrive with a valid
// session cookie, and puts the operator it did arrive with on the request
// context for the handlers to read with [operatorFrom].
//
// Authentication runs even for the allowlisted routes, because /api/session has
// to report who is signed in and the alternative -- a handler reading the
// cookie for itself -- is a second implementation of the thing that decides who
// somebody is. What the allowlist changes is only whether the absence of an
// operator is refused.
//
// Expiry and revocation are not checked here: [store.Store.SessionOperator]
// answers [store.ErrNotFound] for a session that has run out and finds nothing
// for one that was deleted, which is what makes signing out take effect at the
// database rather than at the browser's willingness to forget a cookie.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if operator, ok := s.authenticate(r); ok {
			r = r.WithContext(withOperator(r.Context(), operator))
		} else if !unauthenticatedRoutes[r.URL.Path] {
			writeJSON(w, http.StatusUnauthorized, errorBody(notSignedIn))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate reports which operator this request's cookie names, if any.
//
// A missing cookie, a cookie naming nothing, an expired session and a revoked
// one are all "no operator" and none of them is an error worth logging: they
// are what the ordinary lifetime of a session looks like from the server. A
// database that could not be read is logged, because that one is dokkup's own
// failure, and it still authenticates nobody -- failing closed is the only
// direction an authentication check may fail in.
func (s *Server) authenticate(r *http.Request) (store.Operator, bool) {
	if s.cfg.Store == nil {
		return store.Operator{}, false
	}

	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return store.Operator{}, false
	}

	// The token is hashed here and goes no further. The store matches on the
	// hash and never sees the secret the browser holds.
	operator, err := s.cfg.Store.SessionOperator(r.Context(), secret.HashToken(cookie.Value))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Operator{}, false
	case err != nil:
		s.logger.Error("reading the operator holding a session", "error", err)
		return store.Operator{}, false
	}
	return operator, true
}

// requireCSRF rejects a state-changing request that did not carry the header.
//
// It guards the whole API surface rather than one route, so that a mutation
// added to the mux tomorrow inherits it without anybody remembering to. It is
// the second half of a pair: the session cookie is SameSite=Strict, which a
// browser will not attach to a cross-site request at all, and this header is
// what a browser will not attach without a successful preflight.
//
// The refusal is JSON and not [http.Error]'s text, because the client reads one
// field out of every failure -- see web/src/lib/api.ts, which falls back to the
// bare status text and would show an operator "Forbidden" where it promised a
// sentence they can act on.
func requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get(csrfHeader) == "" {
				writeJSON(w, http.StatusForbidden, errorBody("that request is missing the "+csrfHeader+" header"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (s *Server) withRequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		// The path is logged, never the query string or body: those are where
		// secrets end up, and a log is one of the places SECURITY.md promises
		// they will not appear.
		s.logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start),
		)
	})
}
