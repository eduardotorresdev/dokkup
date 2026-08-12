package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// requireCSRF now guards the whole API surface rather than the one route it was
// written for. The table below is unchanged by that move -- the rule is still
// "a state-changing method needs the header" -- and what the move did change is
// tested underneath it: which paths the rule is applied to at all.
func TestRequireCSRF(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		method     string
		header     string
		wantStatus int
		wantCalled bool
	}{
		{"GET needs no header", http.MethodGet, "", http.StatusOK, true},
		{"HEAD needs no header", http.MethodHead, "", http.StatusOK, true},
		{"OPTIONS needs no header", http.MethodOptions, "", http.StatusOK, true},
		{"POST without the header is refused", http.MethodPost, "", http.StatusForbidden, false},
		{"PUT without the header is refused", http.MethodPut, "", http.StatusForbidden, false},
		{"PATCH without the header is refused", http.MethodPatch, "", http.StatusForbidden, false},
		{"DELETE without the header is refused", http.MethodDelete, "", http.StatusForbidden, false},
		{"POST with the header passes", http.MethodPost, "1", http.StatusOK, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			called := false
			handler := requireCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequestWithContext(t.Context(), tc.method, "/api/anything", nil)
			if tc.header != "" {
				req.Header.Set(csrfHeader, tc.header)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if called != tc.wantCalled {
				t.Errorf("next handler called = %v, want %v", called, tc.wantCalled)
			}
		})
	}
}

// apiOnly is what keeps the two guards off the catch-all that serves the
// frontend. Asserted rather than assumed, because getting it wrong in the
// generous direction publishes the API and getting it wrong in the strict
// direction makes the application shell unreachable -- and the shell is what a
// browser needs before anybody can sign in to anything.
func TestApiOnlyAppliesTheGuardToTheAPIAndNothingElse(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		path       string
		wantStatus int
	}{
		"a route under the API prefix is guarded": {"/api/anything", http.StatusForbidden},
		"the application shell is not":            {"/some/route/the/frontend/owns", http.StatusOK},
		"nor is the root":                         {"/", http.StatusOK},
		// "/api" without the slash is not the API: no route is registered at
		// it, so it is a path the frontend owns like any other.
		"nor is a path that merely starts with the letters": {"/apiary", http.StatusOK},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			handler := apiOnly(requireCSRF)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("POST %s status = %d, want %d", tc.path, rec.Code, tc.wantStatus)
			}
		})
	}
}

// The allowlist is the whole of dokkup's authorisation, so what it contains is
// asserted exactly rather than sampled.
//
// Removing an entry is caught by the tests that use that route. Adding one is
// not caught by anything else at all: a route added to this map is published to
// every stranger who can reach the port, silently, and it is the one edit here
// that no failing test would ever announce. The point of choosing an allowlist
// was to make widening it deliberate and visible; this is what makes it
// visible. A path added below is a path somebody decided to publish.
func TestTheAllowlistIsExactlyTheFourRoutesThatMayBeReachedWithoutASession(t *testing.T) {
	t.Parallel()

	want := map[string]bool{
		"/api/health":  true,
		"/api/session": true,
		"/api/setup":   true,
		"/api/signin":  true,
	}

	for path := range unauthenticatedRoutes {
		if !want[path] {
			t.Errorf("%s may be reached without signing in, and nothing decided that it should: "+
				"either it belongs in this test with a reason, or the allowlist is publishing it "+
				"by accident", path)
		}
	}
	for path := range want {
		if !unauthenticatedRoutes[path] {
			t.Errorf("%s needs a session, and a caller who has not signed in cannot get one "+
				"through it", path)
		}
	}
}
