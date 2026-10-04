package share

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/pool"
)

type fakeAccess struct {
	grants map[string]ShareGrants
	err    error
}

func (f *fakeAccess) ShareGrants(context.Context) (map[string]ShareGrants, error) {
	return f.grants, f.err
}

func smbSection(t *testing.T, svc *Service, name string) string {
	t.Helper()
	conf := readFile(t, filepath.Join(svc.Gen.Root, config.PathSamba))
	_, rest, ok := strings.Cut(conf, "\n["+name+"]\n")
	if !ok {
		t.Fatalf("no [%s] in smb.conf:\n%s", name, conf)
	}
	section, _, _ := strings.Cut(rest, "\n[")
	section, _, _ = strings.Cut(section, "\ninclude = ")
	return section
}

func TestEffectiveAccess(t *testing.T) {
	g := ShareGrants{
		Users: map[string]string{"alice": "read-write", "bob": "none", "dave": "read-only", "gina": "read-write"},
		Groups: []GroupGrant{
			{Access: "read-only", Members: []string{"bob", "carol", "dave", "gina", "hank"}},
			{Access: "read-write", Members: []string{"carol", "ivan"}},
			{Access: "none", Members: []string{"ivan", "jane"}},
		},
	}
	valid, write := effectiveAccess(g)
	if want := []string{"alice", "carol", "dave", "gina", "hank", "ivan"}; !reflect.DeepEqual(valid, want) {
		t.Errorf("valid = %v, want %v (bob's own none beats his group's read-only, jane's only grant is none, erin has none)", valid, want)
	}
	if want := []string{"alice", "carol", "gina", "ivan"}; !reflect.DeepEqual(write, want) {
		t.Errorf("write = %v, want %v (the widest group grant wins; dave's own read-only stays read-only)", write, want)
	}
	if valid, write := effectiveAccess(ShareGrants{}); len(valid) != 0 || len(write) != 0 {
		t.Errorf("no grants gave valid %v write %v", valid, write)
	}
}

// The unauthorised-access scenario: before smb.conf carried the grants, a
// user denied the share (own none), one denied by no grant at all and one
// granted only through a group were all let in alike.
func TestRefreshAccess_RendersEachUserAsGranted(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	svc.Access = &fakeAccess{grants: map[string]ShareGrants{"media": {
		Users:  map[string]string{"alice": "read-write", "bob": "none"},
		Groups: []GroupGrant{{Access: "read-only", Members: []string{"bob", "carol"}}},
	}}}

	if err := svc.RefreshAccess(ctx); err != nil {
		t.Fatal(err)
	}
	section := smbSection(t, svc, "media")
	for _, want := range []string{"read only = yes", "valid users = alice carol", "write list = alice"} {
		if !strings.Contains(section, "\n   "+want+"\n") {
			t.Errorf("[media] lacks %q:\n%s", want, section)
		}
	}
	for _, denied := range []string{"bob", "erin"} {
		if strings.Contains(section, denied) {
			t.Errorf("[media] names %s, who has no access:\n%s", denied, section)
		}
	}
}

func TestShareFiles_NoGrantsMeansNoAccess(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	if _, err := svc.Create(ctx, CreateInput{Name: "public", CacheMode: pool.ArrayOnly, SMB: &SMB{Enabled: true, Guest: true}}); err != nil {
		t.Fatal(err)
	}

	for name, access := range map[string]AccessReader{"no reader": nil, "empty reader": &fakeAccess{}} {
		svc.Access = access
		if err := svc.RefreshAccess(ctx); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if section := smbSection(t, svc, "media"); !strings.Contains(section, "\n   available = no\n") || strings.Contains(section, "valid users") || strings.Contains(section, "read only = no") {
			t.Errorf("%s: a share with no grants must be closed:\n%s", name, section)
		}
		if section := smbSection(t, svc, "public"); !strings.Contains(section, "guest ok = yes") || strings.Contains(section, "available") || strings.Contains(section, "valid users") {
			t.Errorf("%s: a guest share keeps its rendering:\n%s", name, section)
		}
	}
}

func TestRefreshAccess_AFailedGrantReadNeverRendersAnOpenShare(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	svc.Access = &fakeAccess{grants: map[string]ShareGrants{"media": {Users: map[string]string{"alice": "read-write"}}}}
	if err := svc.RefreshAccess(ctx); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, filepath.Join(svc.Gen.Root, config.PathSamba))

	svc.Access = &fakeAccess{err: errors.New("database is locked")}
	err := svc.RefreshAccess(ctx)
	if err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("RefreshAccess = %v, want the read failure", err)
	}
	if after := readFile(t, filepath.Join(svc.Gen.Root, config.PathSamba)); after != before {
		t.Errorf("smb.conf changed although the grants could not be read:\n%s", after)
	}
	if _, err := svc.Update(ctx, "media", UpdateInput{SMB: &SMB{Enabled: true, Browseable: false}}); err == nil {
		t.Error("a share update went through although the grants could not be read")
	}
}

// A share edit, which regenerates smb.conf from the same state, keeps the
// grants rather than reverting the share to open.
func TestShareUpdate_KeepsTheGrants(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	svc.Access = &fakeAccess{grants: map[string]ShareGrants{"media": {Users: map[string]string{"alice": "read-only"}}}}
	createTestShare(t, svc, "media", pool.ArrayOnly)
	if section := smbSection(t, svc, "media"); !strings.Contains(section, "valid users = alice") {
		t.Fatalf("create dropped the grants:\n%s", section)
	}
	if _, err := svc.Update(ctx, "media", UpdateInput{SMB: &SMB{Enabled: true, Browseable: false}}); err != nil {
		t.Fatal(err)
	}
	section := smbSection(t, svc, "media")
	if !strings.Contains(section, "valid users = alice") || strings.Contains(section, "write list") || !strings.Contains(section, "browseable = no") {
		t.Errorf("update lost the grants:\n%s", section)
	}
}

// pausingAccess reads grants like fakeAccess, but its first read stops after
// it has taken its copy until the test lets it go, the way a slow read leaves
// a writer holding grants a later change has already replaced.
type pausingAccess struct {
	mu      sync.Mutex
	grants  map[string]ShareGrants
	reading chan struct{}
	release chan struct{}
	paused  bool
}

func (p *pausingAccess) ShareGrants(context.Context) (map[string]ShareGrants, error) {
	p.mu.Lock()
	copied := p.grants
	first := !p.paused
	p.paused = true
	p.mu.Unlock()
	if first {
		close(p.reading)
		<-p.release
	}
	return copied, nil
}

func (p *pausingAccess) set(grants map[string]ShareGrants) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.grants = grants
}

// A revocation reported as applied must not be undone by a share update that
// read the grants before it and writes its smb.conf after it.
func TestRefreshAccess_ARevocationIsNotUndoneByAConcurrentShareUpdate(t *testing.T) {
	ctx, svc, _, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	both := map[string]ShareGrants{"media": {Users: map[string]string{"alice": "read-write", "bob": "read-write"}}}
	access := &pausingAccess{grants: both, reading: make(chan struct{}), release: make(chan struct{})}
	svc.Access = access

	updated := make(chan error, 1)
	go func() {
		_, err := svc.Update(ctx, "media", UpdateInput{SMB: &SMB{Enabled: true, Browseable: false}})
		updated <- err
	}()
	<-access.reading

	access.set(map[string]ShareGrants{"media": {Users: map[string]string{"alice": "read-write", "bob": "none"}}})
	refreshed := make(chan error, 1)
	go func() { refreshed <- svc.RefreshAccess(ctx) }()
	var refreshErr error
	done := false
	select {
	case refreshErr = <-refreshed:
		done = true
	case <-time.After(time.Second):
	}
	close(access.release)

	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if !done {
		refreshErr = <-refreshed
	}
	if refreshErr != nil {
		t.Fatal(refreshErr)
	}
	if section := smbSection(t, svc, "media"); strings.Contains(section, "bob") || !strings.Contains(section, "valid users = alice") {
		t.Errorf("bob's revoked access is back in smb.conf:\n%s", section)
	}
}
