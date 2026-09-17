package main

import (
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func testWebRoot() fstest.MapFS {
	return fstest.MapFS{
		"index.html":     {Data: []byte("<html>spa shell</html>")},
		"assets/app.js":  {Data: []byte("console.log('app')")},
		"assets/app.css": {Data: []byte("body{}")},
	}
}

func TestSPAHandlerServesRealAsset(t *testing.T) {
	h := spaHandler(testWebRoot())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/app.js", nil))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "console.log('app')" {
		t.Errorf("body = %q, want the real asset content", rec.Body.String())
	}
}

func TestSPAHandlerFallsBackToIndexForUnknownPath(t *testing.T) {
	h := spaHandler(testWebRoot())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/storage/disks/42", nil))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "<html>spa shell</html>" {
		t.Errorf("body = %q, want index.html so the client-side router can take over", rec.Body.String())
	}
}

func TestSPAHandlerServesIndexAtRoot(t *testing.T) {
	h := spaHandler(testWebRoot())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != 200 || rec.Body.String() != "<html>spa shell</html>" {
		t.Errorf("GET / = %d %q, want 200 index.html", rec.Code, rec.Body.String())
	}
}
