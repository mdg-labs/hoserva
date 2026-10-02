package container

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
)

type memCredentials struct {
	rows map[string]store.RegistryCredential
}

func newMemCredentials() *memCredentials {
	return &memCredentials{rows: map[string]store.RegistryCredential{}}
}

func (m *memCredentials) Put(_ context.Context, registry string, sealed []byte, at time.Time) error {
	m.rows[registry] = store.RegistryCredential{Registry: registry, Sealed: sealed, UpdatedAt: at}
	return nil
}

func (m *memCredentials) Get(_ context.Context, registry string) (store.RegistryCredential, bool, error) {
	row, ok := m.rows[registry]
	return row, ok, nil
}

func (m *memCredentials) List(context.Context) ([]store.RegistryCredential, error) {
	var out []store.RegistryCredential
	for _, row := range m.rows {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Registry < out[j].Registry })
	return out, nil
}

func (m *memCredentials) Delete(_ context.Context, registry string) (bool, error) {
	_, ok := m.rows[registry]
	delete(m.rows, registry)
	return ok, nil
}

// credentialCipher is a reversible SecretCipher whose sealed form never contains
// the plaintext.
type credentialCipher struct{ fail bool }

func (c credentialCipher) Encrypt(p []byte) ([]byte, error) {
	out := make([]byte, len(p))
	for i, b := range p {
		out[i] = b ^ 0x5a
	}
	return out, nil
}

func (c credentialCipher) Decrypt(p []byte) ([]byte, error) {
	if c.fail {
		return nil, errors.New("sealed under another machine key")
	}
	return c.Encrypt(p)
}

func newCredentials() (*RegistryCredentials, *memCredentials) {
	st := newMemCredentials()
	return &RegistryCredentials{Store: st, Cipher: credentialCipher{}, Now: func() time.Time { return time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC) }}, st
}

func TestNormalizeRegistryHost(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io":                   "ghcr.io",
		"GHCR.io":                   "ghcr.io",
		"registry.example.com:5000": "registry.example.com:5000",
		"localhost:5000":            "localhost:5000",
		"index.docker.io":           "docker.io",
		"registry-1.docker.io":      "docker.io",
		"docker.io":                 "docker.io",
		"192.168.1.5:5000":          "192.168.1.5:5000",
	} {
		if got, err := NormalizeRegistryHost(in); err != nil || got != want {
			t.Errorf("NormalizeRegistryHost(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "ghcr", "https://ghcr.io", "ghcr.io/org", "user@ghcr.io", "ghcr.io ", "a b.io", "ghcr.io\n", "5000"} {
		if got, err := NormalizeRegistryHost(in); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("NormalizeRegistryHost(%q) = %q, %v, want ErrInvalidCredential", in, got, err)
		}
	}
}

func TestValidateCredential(t *testing.T) {
	for name, c := range map[string]Credential{
		"no username":         {Password: "p"},
		"no password":         {Username: "u"},
		"a colon in the name": {Username: "a:b", Password: "p"},
		"a newline":           {Username: "u", Password: "p\nq"},
		"a control character": {Username: "u\x00", Password: "p"},
		"a username too long": {Username: strings.Repeat("u", maxCredentialPart+1), Password: "p"},
		"a password too long": {Username: "u", Password: strings.Repeat("p", maxCredentialPart+1)},
	} {
		if err := ValidateCredential(c); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("%s: ValidateCredential = %v, want ErrInvalidCredential", name, err)
		}
	}
	if err := ValidateCredential(Credential{Username: "me", Password: "pa:ss word"}); err != nil {
		t.Errorf("a password with a colon and a space: %v", err)
	}
}

func TestRegistryCredentials_PutSealsAndNeverStoresThePlaintext(t *testing.T) {
	r, st := newCredentials()
	ctx := context.Background()
	if err := r.PutCredential(ctx, "GHCR.io", Credential{Username: "me", Password: "hunter2-password"}); err != nil {
		t.Fatal(err)
	}
	row, ok := st.rows["ghcr.io"]
	if !ok {
		t.Fatalf("rows = %v, want one under the normalised host", st.rows)
	}
	if bytes.Contains(row.Sealed, []byte("hunter2-password")) || bytes.Contains(row.Sealed, []byte("me")) {
		t.Fatalf("the stored value holds the credential in the clear: %q", row.Sealed)
	}
	got, found, err := r.Credential(ctx, "ghcr.io")
	if err != nil || !found || got != (Credential{Username: "me", Password: "hunter2-password"}) {
		t.Fatalf("Credential = %+v, %v, %v", got, found, err)
	}
	if _, found, err := r.Credential(ctx, "quay.io"); found || err != nil {
		t.Fatalf("Credential for another registry = %v, %v, want none", found, err)
	}
	if err := r.PutCredential(ctx, "ghcr.io", Credential{Username: "other", Password: "new"}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := r.Credential(ctx, "ghcr.io"); got.Username != "other" || len(st.rows) != 1 {
		t.Fatalf("a second Put = %+v with %d rows, want it replaced", got, len(st.rows))
	}
}

func TestRegistryCredentials_AnInvalidCredentialStoresNothing(t *testing.T) {
	r, st := newCredentials()
	ctx := context.Background()
	if err := r.PutCredential(ctx, "ghcr", Credential{Username: "u", Password: "p"}); !errors.Is(err, ErrInvalidCredential) {
		t.Errorf("a bad host = %v", err)
	}
	if err := r.PutCredential(ctx, "ghcr.io", Credential{Username: "a:b", Password: "p"}); !errors.Is(err, ErrInvalidCredential) {
		t.Errorf("a bad username = %v", err)
	}
	if len(st.rows) != 0 {
		t.Fatalf("rows = %v after refused puts", st.rows)
	}
}

func TestRegistryCredentials_ListNamesHostsOnlyAndDeleteRemoves(t *testing.T) {
	r, _ := newCredentials()
	ctx := context.Background()
	for _, h := range []string{"quay.io", "ghcr.io"} {
		if err := r.PutCredential(ctx, h, Credential{Username: "u", Password: "secret-" + h}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.ListCredentialRegistries(ctx)
	if err != nil || strings.Join(got, ",") != "ghcr.io,quay.io" {
		t.Fatalf("ListCredentialRegistries = %v, %v", got, err)
	}
	if err := r.DeleteCredential(ctx, "ghcr.io"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteCredential(ctx, "ghcr.io"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("a second delete = %v, want ErrCredentialNotFound", err)
	}
	if err := r.DeleteCredential(ctx, "nonsense"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("delete of a bad host = %v, want ErrInvalidCredential", err)
	}
	if got, _ := r.ListCredentialRegistries(ctx); strings.Join(got, ",") != "quay.io" {
		t.Fatalf("after the delete = %v", got)
	}
}

func TestRegistryCredentials_ACredentialThatCannotBeOpenedIsAnErrorNeverNone(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(*RegistryCredentials, *memCredentials){
		"cleared by a restore": func(r *RegistryCredentials, st *memCredentials) {
			st.rows["ghcr.io"] = store.RegistryCredential{Registry: "ghcr.io"}
		},
		"sealed under another key": func(r *RegistryCredentials, st *memCredentials) {
			r.Cipher = credentialCipher{fail: true}
		},
		"not a credential": func(r *RegistryCredentials, st *memCredentials) {
			sealed, _ := credentialCipher{}.Encrypt([]byte("not json"))
			st.rows["ghcr.io"] = store.RegistryCredential{Registry: "ghcr.io", Sealed: sealed}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, st := newCredentials()
			if err := r.PutCredential(ctx, "ghcr.io", Credential{Username: "u", Password: "p"}); err != nil {
				t.Fatal(err)
			}
			setup(r, st)
			_, found, err := r.Credential(ctx, "ghcr.io")
			if !errors.Is(err, ErrCredentialUnusable) || found {
				t.Fatalf("Credential = found %v, %v, want ErrCredentialUnusable", found, err)
			}
			if !strings.Contains(err.Error(), "registry-credential-set ghcr.io") {
				t.Errorf("the error %q does not name the operation that saves the credential again", err)
			}
		})
	}
}
