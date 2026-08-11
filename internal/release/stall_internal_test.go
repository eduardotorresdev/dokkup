package release

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A server that accepts the connection and then says nothing must not hold a
// root command open forever.
//
// This cannot be a deadline on the caller's context: `dokkup update` hands the
// same context to its rollback, so a context that expired mid-download would
// take the rollback's own systemctl calls with it and report a host that needs
// attention where there is only a slow link. What is refused is silence, which
// leaves a slow-but-arriving transfer alone.
func TestADownloadThatGoesQuietIsGivenUpOn(t *testing.T) {
	t.Parallel()

	// Long enough that the test would hang without the watchdog, and the test's
	// own deadline is what would fail it.
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte("dok"))
			flusher.Flush()
		}
		// Then nothing, ever.
		select {
		case <-blocked:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	// Shortened for the test; the shipped value is a minute, which is a pause
	// nobody would sit through here.
	source := &Source{HTTP: srv.Client(), StallTimeout: 50 * time.Millisecond}

	var sink []byte
	start := time.Now()
	err := source.download(context.Background(), srv.URL, writerTo(&sink))

	if err == nil {
		t.Fatal("a download that stopped arriving was reported as complete")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("waited %v for a 50ms stall timeout", elapsed)
	}
	// The operator has to be able to tell this from a refused connection.
	if !strings.Contains(err.Error(), "nothing arrived") {
		t.Errorf("the error does not say the download went quiet: %v", err)
	}
}

// The other half of the same decision: a transfer that keeps arriving, however
// slowly, is not interrupted. Without this the fix above would just be a
// download size limit wearing a different hat.
func TestASlowDownloadThatKeepsArrivingIsLeftAlone(t *testing.T) {
	t.Parallel()

	const chunks = 8

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("the test server cannot flush, so it cannot dribble")
			return
		}
		for range chunks {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			flusher.Flush()
			select {
			case <-time.After(20 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	// Shorter than the whole transfer, longer than any one gap in it. A total
	// deadline of this length would fail; a stall timeout must not.
	source := &Source{HTTP: srv.Client(), StallTimeout: 80 * time.Millisecond}

	var sink []byte
	if err := source.download(context.Background(), srv.URL, writerTo(&sink)); err != nil {
		t.Fatalf("a slow but arriving download was abandoned: %v", err)
	}
	if len(sink) != chunks {
		t.Errorf("got %d bytes, want %d", len(sink), chunks)
	}
}

// The caller's own cancellation still wins, and is not disguised as a stall.
func TestACancelledDownloadIsReportedAsCancelled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	source := &Source{HTTP: srv.Client()}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	var sink []byte
	err := source.download(ctx, srv.URL, writerTo(&sink))
	if err == nil {
		t.Fatal("a cancelled download was reported as complete")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if strings.Contains(err.Error(), "nothing arrived") {
		t.Errorf("a cancellation was reported as a stall: %v", err)
	}
}

var _ io.Reader = readerFunc(nil)
