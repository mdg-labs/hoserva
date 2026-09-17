package main

import (
	"io/fs"
	"net/http"
)

// spaHandler serves the built React SPA (Q8, doc 01 §1) for every
// non-API path: the real static asset when one exists at the requested
// path, and index.html (so the client-side router can take over) for any
// path that doesn't — /api/… never reaches this handler at all (it is
// mounted separately, under /api/v1, ahead of this one).
func spaHandler(root fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "" {
			path = "/"
		}
		name := path[1:] // fs.FS paths never start with "/"
		if name == "" {
			name = "index.html"
		}
		if f, err := root.Open(name); err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		// No asset at this path — hand the client-side router
		// index.html instead of a 404, without mutating the request the
		// caller still owns. Redirected to "/", not "/index.html"
		// directly: net/http's own FileServer special-cases any request
		// path ending in "/index.html" with a 301 to "./" (its answer to
		// "don't let index.html be reached by its own name"), which
		// would otherwise turn this fallback into a redirect loop instead
		// of serving the SPA shell.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}
