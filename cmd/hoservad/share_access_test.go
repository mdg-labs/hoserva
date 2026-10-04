package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
)

// noChownFS is share.OSFS for a test process that may not chown to the
// shared group.
type noChownFS struct{ share.OSFS }

func (noChownFS) Chown(string, int, int) error { return nil }

type accessWiring struct {
	w         *containersWiringHarness
	generator *cfggen.Generator
	accounts  *share.FakeSambaAccounts
	shares    *share.Service
	smbPath   string
}

// wireShareAccess builds the share service the way main.go does
// (newShareServiceWithAccess over the auth store) and serves it, with the
// auth service, from the harness's daemon server.
func wireShareAccess(t *testing.T) *accessWiring {
	t.Helper()
	w := newContainersWiringHarness(t)
	authStore := api.NewAuthStore(w.db)
	key, err := auth.LoadOrGenerateMachineKey(context.Background(), filepath.Join(w.root, "secret.key"), authStore)
	if err != nil {
		t.Fatal(err)
	}
	authService := api.NewAuthService(authStore, key)
	accounts := share.NewFakeSambaAccounts()
	authService.SambaAccounts = accounts
	generator := cfggen.NewGenerator(filepath.Join(w.root, "etc"))
	shares := newShareServiceWithAccess(store.NewShareStore(w.db), w.arrays, generator, wiringTestMounter{}, nil, authStore)
	shares.FS = noChownFS{}
	w.handler.Shares = shares
	w.handler.Auth = authService
	return &accessWiring{w: w, generator: generator, accounts: accounts, shares: shares, smbPath: filepath.Join(w.root, "etc", "samba", "smb.conf")}
}

func (a *accessWiring) call(t *testing.T, method, path string, body any, want int) []byte {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	status, resp := a.w.doBody(t, method, path, string(raw))
	if status != want {
		t.Fatalf("%s %s = %d %s, want %d", method, path, status, resp, want)
	}
	return resp
}

func (a *accessWiring) id(t *testing.T, resp []byte) string {
	t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil || out.ID == "" {
		t.Fatalf("no id in %s: %v", resp, err)
	}
	return out.ID
}

// section returns the share's smb.conf section, failing when smb.conf is
// missing.
func (a *accessWiring) section(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(a.smbPath)
	if err != nil {
		t.Fatalf("reading the generated smb.conf: %v", err)
	}
	_, rest, ok := strings.Cut(string(raw), "\n["+name+"]\n")
	if !ok {
		t.Fatalf("no [%s] in smb.conf:\n%s", name, raw)
	}
	section, _, _ := strings.Cut(rest, "\n[")
	section, _, _ = strings.Cut(section, "\ninclude = ")
	return section
}

func (a *accessWiring) wantLines(t *testing.T, share string, want, notWant []string) {
	t.Helper()
	section := a.section(t, share)
	for _, w := range want {
		if !strings.Contains(section, "\n   "+w+"\n") {
			t.Errorf("[%s] lacks %q:\n%s", share, w, section)
		}
	}
	for _, n := range notWant {
		if strings.Contains(section, n) {
			t.Errorf("[%s] holds %q:\n%s", share, n, section)
		}
	}
}

// Every operation that changes who may reach a share regenerates smb.conf
// through the daemon's own wiring (#594): the share-side and user-side
// editors, a group's membership, and deleting a user or a group.
func TestShareAccessWiring_GrantChangesRegenerateSMBConf(t *testing.T) {
	a := wireShareAccess(t)
	a.call(t, http.MethodPost, "/shares", map[string]any{"name": "media", "cacheMode": "array-only"}, http.StatusOK)
	a.wantLines(t, "media", []string{"available = no"}, []string{"valid users"})

	alice := a.id(t, a.call(t, http.MethodPost, "/users", map[string]any{"username": "alice"}, http.StatusCreated))
	bob := a.id(t, a.call(t, http.MethodPost, "/users", map[string]any{"username": "bob"}, http.StatusCreated))
	carol := a.id(t, a.call(t, http.MethodPost, "/users", map[string]any{"username": "carol"}, http.StatusCreated))
	family := a.id(t, a.call(t, http.MethodPost, "/user-groups", map[string]any{"name": "family"}, http.StatusCreated))
	a.call(t, http.MethodPut, "/user-groups/"+family+"/members", map[string]any{"userIds": []string{bob, carol}}, http.StatusOK)

	// updateSharePermissions: alice writes, the family group reads, and
	// carol's own none beats the group's read-only.
	a.call(t, http.MethodPut, "/shares/media/permissions", map[string]any{
		"users":  []map[string]string{{"userId": alice, "access": "read-write"}, {"userId": carol, "access": "none"}},
		"groups": []map[string]string{{"groupId": family, "access": "read-only"}},
	}, http.StatusOK)
	a.wantLines(t, "media", []string{"read only = yes", "valid users = alice bob", "write list = alice"}, []string{"carol", "available"})

	// updateUserSharePermissions: bob is promoted from the user side.
	a.call(t, http.MethodPut, "/users/"+bob+"/permissions", map[string]any{
		"permissions": []map[string]string{{"shareName": "media", "access": "read-write"}},
	}, http.StatusOK)
	a.wantLines(t, "media", []string{"valid users = alice bob", "write list = alice bob"}, nil)

	// setUserGroupMembers: carol's own none still wins after she joins and
	// leaves; removing bob from the group changes nothing for bob (own grant).
	a.call(t, http.MethodPut, "/user-groups/"+family+"/members", map[string]any{"userIds": []string{carol}}, http.StatusOK)
	a.wantLines(t, "media", []string{"valid users = alice bob", "write list = alice bob"}, []string{"carol"})

	// A user with no grant of her own gains access through the group, and
	// loses it again when the group is deleted.
	dave := a.id(t, a.call(t, http.MethodPost, "/users", map[string]any{"username": "dave"}, http.StatusCreated))
	a.call(t, http.MethodPut, "/user-groups/"+family+"/members", map[string]any{"userIds": []string{carol, dave}}, http.StatusOK)
	a.wantLines(t, "media", []string{"valid users = alice bob dave", "write list = alice bob"}, nil)
	a.call(t, http.MethodDelete, "/user-groups/"+family, nil, http.StatusNoContent)
	a.wantLines(t, "media", []string{"valid users = alice bob", "write list = alice bob"}, []string{"dave"})

	// deleteUser: bob's grant goes with the account.
	a.call(t, http.MethodDelete, "/users/"+bob, nil, http.StatusNoContent)
	a.wantLines(t, "media", []string{"valid users = alice", "write list = alice"}, []string{"bob"})
}

// A regeneration that fails is returned to the caller, the grant stays
// stored, and the next successful regeneration applies it.
func TestShareAccessWiring_AFailedRegenerationIsReturnedAndTheGrantIsKept(t *testing.T) {
	a := wireShareAccess(t)
	a.call(t, http.MethodPost, "/shares", map[string]any{"name": "media", "cacheMode": "array-only"}, http.StatusOK)
	alice := a.id(t, a.call(t, http.MethodPost, "/users", map[string]any{"username": "alice"}, http.StatusCreated))

	// A directory where smb.conf must be written cannot be replaced.
	if err := os.RemoveAll(a.smbPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a.smbPath, "blocker"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"users": []map[string]string{{"userId": alice, "access": "read-write"}}, "groups": []map[string]string{}}
	resp := a.call(t, http.MethodPut, "/shares/media/permissions", body, http.StatusInternalServerError)
	if !strings.Contains(string(resp), "smb_regeneration_failed") || !strings.Contains(string(resp), "smb.conf could not be regenerated") {
		t.Errorf("the failure does not say what happened: %s", resp)
	}
	got := a.call(t, http.MethodGet, "/shares/media/permissions", nil, http.StatusOK)
	if !strings.Contains(string(got), fmt.Sprintf(`"userId":"%s"`, alice)) {
		t.Errorf("the stored grant was lost: %s", got)
	}

	if err := os.RemoveAll(a.smbPath); err != nil {
		t.Fatal(err)
	}
	a.call(t, http.MethodPut, "/shares/media/permissions", body, http.StatusOK)
	a.wantLines(t, "media", []string{"valid users = alice", "write list = alice"}, nil)
}
