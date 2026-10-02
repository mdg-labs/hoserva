package migrate

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// UploadBodyLimit is the largest request body the scan upload takes: a zip of
// MaxZipBytes plus its multipart framing, which is a few hundred bytes.
const UploadBodyLimit = MaxZipBytes + 1<<20

// UploadReadTimeout bounds how long the whole upload may take to arrive, which
// the servers' short header-and-body read timeout is far too brief for.
const UploadReadTimeout = time.Hour

// LimitUploadBody serves next with the request body capped at limit and, once
// the body is first read, the connection's read deadline moved out to timeout
// (none when timeout is 0). The deadline moves at the first read, not on
// arrival: the generated server authenticates a request before it reads a body,
// so an unauthenticated request is answered without its body ever being read,
// and never holds a connection for the longer deadline.
func LimitUploadBody(next http.Handler, limit int64, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if timeout > 0 {
			r.Body = &extendedReadBody{ReadCloser: r.Body, rc: http.NewResponseController(w), timeout: timeout}
		}
		next.ServeHTTP(w, r)
	})
}

type extendedReadBody struct {
	io.ReadCloser
	rc      *http.ResponseController
	timeout time.Duration
	started bool
}

func (b *extendedReadBody) Read(p []byte) (int, error) {
	if !b.started {
		b.started = true
		// Not every transport can move the deadline; the server's own then stays.
		_ = b.rc.SetReadDeadline(time.Now().Add(b.timeout))
	}
	return b.ReadCloser.Read(p)
}

// IsUploadTooLarge reports whether err is the request body going past
// UploadBodyLimit, which the API answers like a zip over MaxZipBytes.
func IsUploadTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	return errors.As(err, &tooLarge)
}

// WriteZipTooLarge answers 413 zip_too_large in the API's error shape.
func WriteZipTooLarge(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": "zip_too_large", "message": ErrZipTooLarge.Error()})
}
