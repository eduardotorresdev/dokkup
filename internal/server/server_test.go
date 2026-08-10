package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/dokku"
	"github.com/eduardotorresdev/dokkup/internal/server"
)

func newTestServer(t *testing.T, cfg server.Config) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(server.New(cfg).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestHealthReportsTheDokkuVersionWhenDokkuAnswers(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()
	fake.DokkuVersion = "0.38.7"
	srv := newTestServer(t, server.Config{Dokku: fake, Mode: server.ModePublished})

	body := getJSON(t, srv.URL+"/api/health", http.StatusOK)

	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
	if body["dokku"] != "0.38.7" {
		t.Errorf("dokku = %v, want 0.38.7", body["dokku"])
	}
}

func TestHealthReportsUnavailableWhenDokkuCannotBeReached(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()
	fake.Err = io.ErrUnexpectedEOF
	srv := newTestServer(t, server.Config{Dokku: fake, Mode: server.ModePublished})

	body := getJSON(t, srv.URL+"/api/health", http.StatusServiceUnavailable)

	if body["status"] != "degraded" {
		t.Errorf("status = %v, want degraded", body["status"])
	}
}

func TestSessionReportsOwnerOnlyInIPMode(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, server.Config{Dokku: dokku.NewFake(), Mode: server.ModeIP})

	body := getJSON(t, srv.URL+"/api/session", http.StatusOK)

	if body["ownerOnly"] != true {
		t.Errorf("ownerOnly = %v, want true: IP mode must refuse additional operators", body["ownerOnly"])
	}
	if body["mode"] != "ip" {
		t.Errorf("mode = %v, want ip", body["mode"])
	}
}

func TestSessionAllowsMoreThanOneOperatorWhenPublished(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, server.Config{Dokku: dokku.NewFake(), Mode: server.ModePublished})

	body := getJSON(t, srv.URL+"/api/session", http.StatusOK)

	if body["ownerOnly"] != false {
		t.Errorf("ownerOnly = %v, want false", body["ownerOnly"])
	}
}

func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, server.Config{Dokku: dokku.NewFake()})

	resp, err := http.Get(srv.URL + "/api/health")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
	}
	for header, value := range want {
		if got := resp.Header.Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp == "" {
		t.Error("Content-Security-Policy is missing")
	}
}

func getJSON(t *testing.T, url string, wantStatus int) map[string]any {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s status = %d, want %d", url, resp.StatusCode, wantStatus)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding %s: %v", url, err)
	}
	return body
}
