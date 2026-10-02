package migrate

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// slowUpload sends a body that takes about 600 ms to arrive to a server whose
// own read timeout is 250 ms, and returns what the handler read and its error.
func slowUpload(t *testing.T, timeout time.Duration) (int, error) {
	t.Helper()
	type result struct {
		n   int
		err error
	}
	got := make(chan result, 1)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		got <- result{len(b), err}
	})
	ts := httptest.NewUnstartedServer(LimitUploadBody(next, 1<<20, timeout))
	ts.Config.ReadTimeout = 250 * time.Millisecond
	ts.Start()
	defer ts.Close()

	pr, pw := io.Pipe()
	go func() {
		for range 4 {
			_, _ = pw.Write([]byte(strings.Repeat("x", 100)))
			time.Sleep(150 * time.Millisecond)
		}
		_ = pw.Close()
	}()
	req, err := http.NewRequest(http.MethodPost, ts.URL, pr)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
	r := <-got
	return r.n, r.err
}

func TestLimitUploadBody_MovesTheReadDeadlineOutForTheUpload(t *testing.T) {
	if n, err := slowUpload(t, 0); err == nil || n == 400 {
		t.Fatalf("without the longer deadline the slow upload read %d bytes (%v), want the server's own read timeout to cut it", n, err)
	}
	if n, err := slowUpload(t, 10*time.Second); err != nil || n != 400 {
		t.Errorf("with the longer deadline the slow upload read %d bytes (%v), want all 400", n, err)
	}
}

func TestLimitUploadBody_RefusesABodyPastTheLimit(t *testing.T) {
	var readErr error
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.Copy(io.Discard, r.Body)
		if IsUploadTooLarge(readErr) {
			WriteZipTooLarge(w)
		}
	})
	ts := httptest.NewServer(LimitUploadBody(next, 1000, time.Minute))
	defer ts.Close()
	resp, err := http.Post(ts.URL, "application/octet-stream", strings.NewReader(strings.Repeat("x", 1001)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(body), `"zip_too_large"`) {
		t.Errorf("a body one byte over the limit = %d %s, want 413 zip_too_large", resp.StatusCode, body)
	}
	if !IsUploadTooLarge(readErr) {
		t.Errorf("the read error = %v, want the body limit", readErr)
	}
}
