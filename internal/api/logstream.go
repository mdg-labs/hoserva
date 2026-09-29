package api

import (
	"net/http"
	"strings"
)

// FlushLogStream wraps next so a container's log response is flushed as
// it is written. The generated server copies the log reader straight into
// the response, which net/http buffers; without a flush after every write
// a followed log would show nothing until the buffer filled or the
// container exited. prefix is the API path prefix ("/api/v1").
func FlushLogStream(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && isAppLogsPath(prefix, r.URL.Path) {
			if f, ok := w.(http.Flusher); ok {
				w = &flushingWriter{ResponseWriter: w, flusher: f}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isAppLogsPath matches prefix + "/apps/{id}/logs": a container ID or name
// never contains a slash.
func isAppLogsPath(prefix, path string) bool {
	rest, ok := strings.CutPrefix(path, prefix+"/apps/")
	if !ok {
		return false
	}
	id, ok := strings.CutSuffix(rest, "/logs")
	return ok && id != "" && !strings.Contains(id, "/")
}

type flushingWriter struct {
	http.ResponseWriter
	flusher http.Flusher
}

func (w *flushingWriter) WriteHeader(code int) {
	w.ResponseWriter.WriteHeader(code)
	w.flusher.Flush()
}

func (w *flushingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.flusher.Flush()
	return n, err
}

func (w *flushingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
