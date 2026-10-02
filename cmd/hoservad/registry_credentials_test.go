package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/acme"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"
)

const registryPassword = "registry-password-never-to-be-returned"

// The credentials are reachable the way main.go wires them: the operations
// over the daemon's socket, the value sealed in the database, carried in the
// archive's secrets, and presented by the registry client the update check
// uses.
func TestWireRegistryCredentials_AreReachableSealedBackedUpAndUsedByTheCheck(t *testing.T) {
	ctx := context.Background()
	box := newWiredInstall(t)
	populateSourceInstall(t, box)
	registry := wireRegistryCredentials(box.handler, store.NewRegistryCredentialStore(box.db), box.key)
	srv := box.serve(t)
	client := srv.generated()

	req := &apiv1.PutRegistryCredentialRequest{Username: "me", Password: registryPassword}
	if err := client.PutRegistryCredential(ctx, req, apiv1.PutRegistryCredentialParams{Registry: "GHCR.io"}); err != nil {
		t.Fatalf("PutRegistryCredential: %v", err)
	}
	if err := client.PutRegistryCredential(ctx, &apiv1.PutRegistryCredentialRequest{Username: "a:b", Password: "x"}, apiv1.PutRegistryCredentialParams{Registry: "ghcr.io"}); err == nil {
		t.Error("a username with a colon was accepted")
	}
	list, err := client.ListRegistryCredentials(ctx)
	if err != nil || len(list.Registries) != 1 || list.Registries[0] != "ghcr.io" {
		t.Fatalf("ListRegistryCredentials = %+v, %v, want the one host", list, err)
	}
	if raw, _ := list.MarshalJSON(); bytes.Contains(raw, []byte(registryPassword)) || bytes.Contains(raw, []byte("me")) {
		t.Fatalf("the listing carries the credential: %s", raw)
	}

	var sealed []byte
	if err := box.db.QueryRow(`SELECT credential FROM registry_credentials WHERE registry = 'ghcr.io'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(registryPassword)) {
		t.Fatal("the credential is stored in the clear")
	}
	if plain, err := box.key.Decrypt(sealed); err != nil || !strings.Contains(string(plain), registryPassword) {
		t.Fatalf("the stored value does not open under the machine key: %v", err)
	}

	secrets, err := backupSecretSource(box.settings, acme.NewStore(box.db), api.NewUPSStore(box.db), api.NewBackupDestinationStore(box.db), notify.NewStore(box.db), store.NewRegistryCredentialStore(box.db)).DatabaseSecrets(ctx)
	if err != nil || len(secrets) != 1 || secrets[0].Table != "registry_credentials" || secrets[0].Column != "credential" || secrets[0].RowID != "ghcr.io" || !bytes.Equal(secrets[0].Ciphertext, sealed) {
		t.Fatalf("the archive's secrets = %+v, %v, want the credential's sealed value under its own row", secrets, err)
	}

	var mu sync.Mutex
	var gotAuth []string
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("me:"+registryPassword)) {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
	}))
	t.Cleanup(tls.Close)
	transport := tls.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, tls.Listener.Addr().String())
	}
	if err := client.PutRegistryCredential(ctx, req, apiv1.PutRegistryCredentialParams{Registry: "example.com"}); err != nil {
		t.Fatal(err)
	}
	registry.Client = &http.Client{Transport: transport}
	if _, err := registry.ManifestDigest(ctx, container.ImageRef{Registry: "example.com", Repository: "acme/app", Tag: "latest"}); err != nil {
		t.Fatalf("the update check's registry client did not log in with the saved credential: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotAuth) != 2 || gotAuth[0] != "" {
		t.Fatalf("Authorization headers sent = %q, want an anonymous request and then the login", gotAuth)
	}

	if err := client.DeleteRegistryCredential(ctx, apiv1.DeleteRegistryCredentialParams{Registry: "ghcr.io"}); err != nil {
		t.Fatalf("DeleteRegistryCredential: %v", err)
	}
	var status *apiv1.ErrorStatusCode
	if err := client.DeleteRegistryCredential(ctx, apiv1.DeleteRegistryCredentialParams{Registry: "ghcr.io"}); !errors.As(err, &status) || status.StatusCode != http.StatusNotFound || status.Response.Code != "registry_credential_not_found" {
		t.Fatalf("a second delete = %v, want 404 registry_credential_not_found", err)
	}
}
