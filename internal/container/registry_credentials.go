package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/distribution/reference"

	"github.com/mdg-labs/hoserva/internal/store"
)

// Credential is the login the update check presents to one registry.
type Credential struct {
	Username string
	Password string
}

// Failures of a registry login. Neither is ever answered by asking the
// registry anonymously instead.
var (
	// ErrCredentialUnusable is a saved credential that cannot be presented:
	// it cannot be opened (cleared by a restore, sealed under another key),
	// or the registry's address would send it over plain HTTP or to a host
	// other than the registry.
	ErrCredentialUnusable = errors.New("container: the saved registry credential cannot be used")
	// ErrCredentialRejected is a registry that refused the credential.
	ErrCredentialRejected = errors.New("container: the registry refused the saved credential")
	// ErrCredentialNotFound is DeleteCredential's refusal of a registry
	// that has none.
	ErrCredentialNotFound = errors.New("container: no credential is saved for that registry")
	// ErrInvalidCredential is a credential or registry host that is not
	// acceptable; its message says why.
	ErrInvalidCredential = errors.New("container: invalid registry credential")
)

// maxCredentialPart bounds the username and the password.
const maxCredentialPart = 1024

// CredentialSource looks up the credential for a registry host as an
// ImageRef names it. A registry with none is (Credential{}, false, nil);
// a credential that is saved but cannot be used is an error.
type CredentialSource interface {
	Credential(ctx context.Context, registry string) (Credential, bool, error)
}

// CredentialStore is the registry_credentials table
// (store.RegistryCredentialStore).
type CredentialStore interface {
	Put(ctx context.Context, registry string, sealed []byte, at time.Time) error
	Get(ctx context.Context, registry string) (store.RegistryCredential, bool, error)
	List(ctx context.Context) ([]store.RegistryCredential, error)
	Delete(ctx context.Context, registry string) (bool, error)
}

var _ CredentialStore = (*store.RegistryCredentialStore)(nil)

// RegistryCredentials is the credentials the daily update check logs in to
// registries with (Q81): each sealed under the machine key (Q28) and never
// returned. *auth.MachineKey is the Cipher.
type RegistryCredentials struct {
	Store  CredentialStore
	Cipher SecretCipher
	// Now reports the time stored with a credential; nil means time.Now.
	Now func() time.Time
}

var _ CredentialSource = (*RegistryCredentials)(nil)

// NormalizeRegistryHost is the key a registry goes by: the host, with its
// port when it has one, as an image reference names it and in lower case,
// so that "index.docker.io" is "docker.io".
func NormalizeRegistryHost(host string) (string, error) {
	h := strings.ToLower(host)
	switch h {
	case "index.docker.io", "registry-1.docker.io":
		return "docker.io", nil
	}
	if h == "" || strings.ContainsFunc(h, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", fmt.Errorf("%w: a registry is a host name such as ghcr.io or registry.example.com:5000", ErrInvalidCredential)
	}
	named, err := reference.ParseNormalizedNamed(h + "/x")
	if err != nil || reference.Domain(named) != h {
		return "", fmt.Errorf("%w: %q is not a registry host name such as ghcr.io or registry.example.com:5000", ErrInvalidCredential, host)
	}
	return h, nil
}

// credentialKey is the key an image's registry looks its credential up by:
// the one PutCredential saved it under.
func credentialKey(registry string) string {
	if key, err := NormalizeRegistryHost(registry); err == nil {
		return key
	}
	return strings.ToLower(registry)
}

// ValidateCredential refuses a credential that cannot be sent as HTTP basic
// authentication.
func ValidateCredential(c Credential) error {
	if c.Username == "" || c.Password == "" {
		return fmt.Errorf("%w: a username and a password are both required", ErrInvalidCredential)
	}
	if len(c.Username) > maxCredentialPart || len(c.Password) > maxCredentialPart {
		return fmt.Errorf("%w: the username and the password are each at most %d bytes", ErrInvalidCredential, maxCredentialPart)
	}
	if strings.Contains(c.Username, ":") {
		return fmt.Errorf("%w: a username cannot contain a colon", ErrInvalidCredential)
	}
	for _, s := range []string{c.Username, c.Password} {
		if strings.ContainsFunc(s, unicode.IsControl) {
			return fmt.Errorf("%w: the username and the password cannot contain control characters", ErrInvalidCredential)
		}
	}
	return nil
}

type sealedCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (r *RegistryCredentials) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// PutCredential saves the credential for registry, replacing one it already
// has.
func (r *RegistryCredentials) PutCredential(ctx context.Context, registry string, c Credential) error {
	key, err := NormalizeRegistryHost(registry)
	if err != nil {
		return err
	}
	if err := ValidateCredential(c); err != nil {
		return err
	}
	plain, err := json.Marshal(sealedCredential(c))
	if err != nil {
		return fmt.Errorf("encoding the credential of registry %s: %w", key, err)
	}
	sealed, err := r.Cipher.Encrypt(plain)
	if err != nil {
		return fmt.Errorf("sealing the credential of registry %s: %w", key, err)
	}
	return r.Store.Put(ctx, key, sealed, r.now())
}

// DeleteCredential removes the credential for registry, or is
// ErrCredentialNotFound.
func (r *RegistryCredentials) DeleteCredential(ctx context.Context, registry string) error {
	key, err := NormalizeRegistryHost(registry)
	if err != nil {
		return err
	}
	found, err := r.Store.Delete(ctx, key)
	if err != nil {
		return err
	}
	if !found {
		return ErrCredentialNotFound
	}
	return nil
}

// ListCredentialRegistries returns the registry hosts that have a
// credential, sorted. It opens none of them.
func (r *RegistryCredentials) ListCredentialRegistries(ctx context.Context) ([]string, error) {
	rows, err := r.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Registry)
	}
	return out, nil
}

// Credential implements CredentialSource. A row that holds nothing, or that
// the machine key does not open, is ErrCredentialUnusable: the registry is
// never asked anonymously instead.
func (r *RegistryCredentials) Credential(ctx context.Context, registry string) (Credential, bool, error) {
	key := credentialKey(registry)
	row, found, err := r.Store.Get(ctx, key)
	if err != nil {
		return Credential{}, false, fmt.Errorf("reading the credential saved for %s: %w", key, err)
	}
	if !found {
		return Credential{}, false, nil
	}
	unusable := func(why string) error {
		return fmt.Errorf("%w: the credential saved for %s %s; add it again with `hoserva app registry-credential-set %s`", ErrCredentialUnusable, key, why, key)
	}
	if len(row.Sealed) == 0 {
		return Credential{}, false, unusable("was cleared by a restore")
	}
	plain, err := r.Cipher.Decrypt(row.Sealed)
	if err != nil {
		return Credential{}, false, unusable("cannot be opened with this installation's machine key")
	}
	var c sealedCredential
	if err := json.Unmarshal(plain, &c); err != nil || ValidateCredential(Credential(c)) != nil {
		return Credential{}, false, unusable("is not readable")
	}
	return Credential(c), true, nil
}
