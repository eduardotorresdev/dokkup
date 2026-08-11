package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// staticFiles holds the built frontend.
//
// The contents are produced by `make web` and are not checked in; only the
// placeholder beside them is, so that `go build` and `go vet` work in a fresh
// clone without building the frontend first. A binary built that way serves no
// interface -- use `make build`, which does both in order.
//
// The pattern covers `static` rather than `static/dist` because the frontend
// build empties its own output directory: a placeholder inside it would be
// deleted by every build, and an embed pattern that matches nothing is a
// compile error rather than a missing interface.
//
//go:embed all:static
var staticFiles embed.FS

// staticHandler serves the frontend, falling back to the application shell for
// any path the client owns.
func staticHandler() http.Handler {
	dist, err := fs.Sub(staticFiles, "static/dist")
	if err != nil {
		panic("embedded frontend missing: run `make web` before building: " + err.Error())
	}

	files := http.FileServerFS(dist)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A request for a path with no file behind it is a client-side route,
		// not a missing resource, so it gets the shell. Requests that look like
		// asset fetches get an honest 404 instead, so a broken asset reference
		// fails visibly rather than silently returning HTML.
		if _, err := fs.Stat(dist, strings.TrimPrefix(r.URL.Path, "/")); err != nil {
			if isAssetRequest(r.URL.Path) {
				http.NotFound(w, r)
				return
			}
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}

		files.ServeHTTP(w, r)
	})
}

func isAssetRequest(path string) bool {
	dot := strings.LastIndex(path, ".")
	slash := strings.LastIndex(path, "/")
	return dot > slash
}
