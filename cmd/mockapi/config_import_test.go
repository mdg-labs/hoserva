package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	ht "github.com/ogen-go/ogen/http"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func TestMockConfigImport_RefusesAnUploadOverTheSizeLimit(t *testing.T) {
	prev := maxMockArchiveBytes
	maxMockArchiveBytes = 16
	t.Cleanup(func() { maxMockArchiveBytes = prev })

	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		size     int
		wantCode string
		wantHTTP int
	}{
		{"over the limit", 17, "archive_too_large", 413},
		{"at the limit", 16, "invalid_archive", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := ht.MultipartFile{File: bytes.NewReader(make([]byte, tc.size))}
			_, previewErr := h.PreviewConfigImport(ctx, &apiv1.PreviewConfigImportReq{Archive: archive})
			importErr := h.ImportConfig(ctx, &apiv1.ImportConfigReq{Confirm: true, Archive: ht.MultipartFile{File: bytes.NewReader(make([]byte, tc.size))}})
			for op, err := range map[string]error{"PreviewConfigImport": previewErr, "ImportConfig": importErr} {
				var me *mockError
				if !errors.As(err, &me) || me.code != tc.wantCode || me.statusCode != tc.wantHTTP {
					t.Errorf("%s = %v, want %d %s", op, err, tc.wantHTTP, tc.wantCode)
				}
			}
		})
	}
}
