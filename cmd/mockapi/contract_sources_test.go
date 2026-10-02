package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/template"
)

// contractSourceArchive is the one empty catalog every scripted source host
// serves, and its signature by the mock's imaginary publisher key.
func contractSourceArchive(t *testing.T) (archive, sig []byte) {
	t.Helper()
	index := `{"schema":1,"serial":1,"templates":[]}`
	var tarball bytes.Buffer
	tw := tar.NewWriter(&tarball)
	if err := tw.WriteHeader(&tar.Header{Name: "index.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(index))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(index)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(tarball.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), ed25519.Sign(mockSourceKey, out.Bytes())
}

// scriptedSourceHosts serves every https catalog host the contract adds
// without touching the network: mockDownHost fails, every other host serves
// the one archive and its signature, with validators so a refresh finds it
// unchanged.
type scriptedSourceHosts struct{ archive, sig []byte }

func (s scriptedSourceHosts) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	switch {
	case req.URL.Hostname() == mockDownHost:
		rec.WriteHeader(http.StatusInternalServerError)
	case strings.HasSuffix(req.URL.Path, "/catalog.tar.zst"):
		rec.Header().Set("ETag", `"contract-1"`)
		if req.Header.Get("If-None-Match") == `"contract-1"` {
			rec.WriteHeader(http.StatusNotModified)
			break
		}
		_, _ = rec.Write(s.archive)
	case strings.HasSuffix(req.URL.Path, "/catalog.tar.zst.sig"):
		_, _ = rec.Write(s.sig)
	default:
		rec.WriteHeader(http.StatusNotFound)
	}
	res := rec.Result()
	res.Request = req
	return res, nil
}

// contractCatalogSources is the production catalog source service in the
// contract rig: the real one over the rig's migrated database, the mock's own
// catalog as the curated one, and the scripted hosts above.
func contractCatalogSources(t *testing.T, db *sql.DB, curated template.CuratedRefresher) *template.Sources {
	t.Helper()
	archive, sig := contractSourceArchive(t)
	sources := &template.Sources{
		Store:          store.NewCatalogSourceStore(db),
		Dir:            filepath.Join(t.TempDir(), "catalog-sources"),
		Curated:        mockCatalog(),
		CuratedRefresh: curated,
		Client:         &http.Client{Transport: scriptedSourceHosts{archive: archive, sig: sig}},
	}
	if err := sources.EnsureCurated(context.Background()); err != nil {
		t.Fatal(err)
	}
	return sources
}
