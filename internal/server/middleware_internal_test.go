package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// requireCSRF guards no route yet, because nothing mutates state yet. It is
// tested now so that the first route to need it inherits a checked guard rather
// than an assumption.
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
