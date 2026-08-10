package server

import (
	"net/http"
	"time"
)

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

// requireCSRF rejects a state-changing request that did not carry the header.
//
// Not yet wired into any route, because no route mutates anything yet. It is
// here so that the first one that does has no excuse.
func requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get(csrfHeader) == "" {
				http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
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
