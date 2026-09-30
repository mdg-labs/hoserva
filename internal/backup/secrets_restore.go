package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"filippo.io/age"
)

var (
	// ErrNoSecrets: the archive has no secrets.age, which is what an archive
	// built without a backup passphrase looks like.
	ErrNoSecrets = errors.New("the archive has no secrets.age")
	// ErrNoIdentity: the archive has no identity.age.
	ErrNoIdentity = errors.New("the archive has no identity.age")
	// ErrPassphraseIncorrect: the passphrase does not open secrets.age or
	// identity.age, whichever was being opened.
	ErrPassphraseIncorrect = errors.New("the passphrase does not open the archive's passphrase-protected files")
	// ErrSecretsUnreadable: secrets.age is not a readable age file, or its
	// content is not what BuildArchive writes.
	ErrSecretsUnreadable = errors.New("secrets.age cannot be read")
	// ErrIdentityUnreadable: identity.age is not a readable age file, or does
	// not hold an age identity.
	ErrIdentityUnreadable = errors.New("identity.age cannot be read")
)

// Secrets is the opened content of an archive's secrets.age.
type Secrets struct {
	payload secretsPayload
}

// ReadSecrets opens the secrets.age of the verified archive tree with
// passphrase (Q28, Q80). ErrNoSecrets says the archive has none,
// ErrPassphraseIncorrect that the passphrase is not the one it was sealed
// under, ErrSecretsUnreadable that the file is damaged; nothing is written.
func ReadSecrets(tree, passphrase string) (*Secrets, error) {
	data, err := os.ReadFile(filepath.Join(tree, "secrets.age"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoSecrets
	}
	if err != nil {
		return nil, fmt.Errorf("reading secrets.age: %w", err)
	}
	payload, err := decryptSecretsAge(data, passphrase)
	if err != nil {
		return nil, err
	}
	return &Secrets{payload: *payload}, nil
}

// ReadIdentity opens the identity.age of the verified archive tree with
// passphrase and returns the onboarding recipient it carries (Q80): the
// private identity, and the public recipient derived from it. ErrNoIdentity
// says the archive has none, ErrPassphraseIncorrect that the passphrase is
// not the one it was sealed under, ErrIdentityUnreadable that the file is
// damaged; nothing is written.
func ReadIdentity(tree, passphrase string) (*Recipient, error) {
	data, err := os.ReadFile(filepath.Join(tree, "identity.age"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoIdentity
	}
	if err != nil {
		return nil, fmt.Errorf("reading identity.age: %w", err)
	}
	plain, err := decryptScrypt(data, passphrase)
	switch {
	case errors.Is(err, ErrPassphraseIncorrect):
		return nil, fmt.Errorf("decrypting identity.age: %w", err)
	case err != nil:
		return nil, fmt.Errorf("decrypting identity.age: %w", errors.Join(ErrIdentityUnreadable, err))
	}
	identity, err := age.ParseX25519Identity(strings.TrimSpace(string(plain)))
	if err != nil {
		// The parser's message can quote part of what it was given.
		return nil, fmt.Errorf("identity.age holds no valid identity: %w", ErrIdentityUnreadable)
	}
	return &Recipient{Public: identity.Recipient().String(), Identity: identity.String()}, nil
}

// StackEnvs is every stack's .env the archive holds, in stack order.
func (s *Secrets) StackEnvs() []StackEnv {
	out := make([]StackEnv, len(s.payload.Stacks))
	for i, e := range s.payload.Stacks {
		out[i] = StackEnv(e)
	}
	slices.SortStableFunc(out, func(a, b StackEnv) int { return strings.Compare(a.Stack, b.Stack) })
	return out
}

// The statuses of an archive's secrets section.
const (
	SecretsNone                = "none"
	SecretsOpened              = "opened"
	SecretsNoPassphrase        = "no_passphrase"
	SecretsPassphraseIncorrect = "passphrase_incorrect"
)

// SecretsOutcome is whether the passphrase-protected section of an archive
// can be restored. Status is secrets.age's and Identity identity.age's, each
// SecretsNone, SecretsOpened, SecretsNoPassphrase or SecretsPassphraseIncorrect
// (an outcome not built by ResolveSecrets has no Identity, which reads as
// SecretsNone). Stacks lists, when secrets.age cannot be opened, the stacks
// of the archive whose .env files would not be restored.
type SecretsOutcome struct {
	Status   string
	Identity string
	Stacks   []string

	secrets       *Secrets
	identity      *Recipient
	archiveStacks []string
	passphrase    string
	fromRequest   bool
}

// Recipient is the archive's onboarding recipient with its private identity,
// recovered from identity.age, or nil when it could not be opened.
func (o SecretsOutcome) Recipient() *Recipient {
	if o.identity == nil {
		return nil
	}
	r := *o.identity
	return &r
}

// OpenedPassphrase is the passphrase that opened secrets.age or identity.age,
// the one a bare-metal restore makes this installation's backup passphrase
// (Q28). ok is false when neither was opened.
func (o SecretsOutcome) OpenedPassphrase() (passphrase string, ok bool) {
	if o.secrets == nil && o.identity == nil {
		return "", false
	}
	return o.passphrase, true
}

// EnvPassphrase is the passphrase that opened the archive's secrets, and
// whether it was the one given with the request rather than the configured
// one, when the import restores at least one stack .env from them. ok is
// false when it restores none: the secrets were not opened, or hold no .env
// for a stack the archive has.
func (o SecretsOutcome) EnvPassphrase() (passphrase string, fromRequest, ok bool) {
	if o.secrets == nil {
		return "", false, false
	}
	for _, e := range o.secrets.StackEnvs() {
		if slices.Contains(o.archiveStacks, e.Stack) {
			return o.passphrase, o.fromRequest, true
		}
	}
	return "", false, false
}

// StackEnvs is what can be restored from the secrets section: nothing unless
// it was opened.
func (o SecretsOutcome) StackEnvs() []StackEnv {
	if o.secrets == nil {
		return nil
	}
	return o.secrets.StackEnvs()
}

// ResolveSecrets decides what an import can restore from the archive tree's
// secrets.age and identity.age. A passphrase given explicitly is the only one
// tried, and one that does not open secrets.age is ErrPassphraseIncorrect, as
// is one that does not open identity.age of an archive with no secrets.age
// (with both, one that opens secrets.age but not identity.age leaves the
// identity unopened: Identity says so); without one the configured backup
// passphrase (src) is tried, and if that is absent or does not open a file
// the outcome says so, so that everything else can still be restored. An
// explicit passphrase for an archive with neither file is not an error: there
// is nothing to check it against. Nothing is written.
func ResolveSecrets(ctx context.Context, tree string, src SecretSource, explicit *string) (SecretsOutcome, error) {
	archiveStacks, err := archiveStackNames(tree)
	if err != nil {
		return SecretsOutcome{}, err
	}
	out := SecretsOutcome{archiveStacks: archiveStacks, Status: SecretsNone, Identity: SecretsNone}
	done := func() (SecretsOutcome, error) {
		if out.Status != SecretsOpened {
			out.Stacks = slices.Clone(archiveStacks)
		}
		return out, nil
	}

	var present [2]bool
	for i, name := range []string{"secrets.age", "identity.age"} {
		_, err := os.Lstat(filepath.Join(tree, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return SecretsOutcome{}, fmt.Errorf("checking for %s: %w", name, err)
		}
		present[i] = err == nil
	}
	hasSecrets, hasIdentity := present[0], present[1]
	if !hasSecrets && !hasIdentity {
		return done()
	}

	var passphrase string
	switch {
	case explicit != nil:
		if *explicit == "" {
			return SecretsOutcome{}, fmt.Errorf("an empty passphrase: %w", ErrPassphraseIncorrect)
		}
		passphrase = *explicit
	case src != nil:
		p, ok, err := src.BackupPassphrase(ctx)
		if err != nil {
			return SecretsOutcome{}, fmt.Errorf("reading the configured backup passphrase: %w", err)
		}
		passphrase = p
		if !ok {
			passphrase = ""
		}
	}
	if passphrase == "" {
		if hasSecrets {
			out.Status = SecretsNoPassphrase
		}
		if hasIdentity {
			out.Identity = SecretsNoPassphrase
		}
		return done()
	}

	if hasSecrets {
		secrets, err := ReadSecrets(tree, passphrase)
		switch {
		case errors.Is(err, ErrPassphraseIncorrect) && explicit == nil:
			out.Status = SecretsPassphraseIncorrect
		case err != nil:
			return SecretsOutcome{}, err
		default:
			out.Status = SecretsOpened
			out.secrets = secrets
		}
	}
	if hasIdentity {
		identity, err := ReadIdentity(tree, passphrase)
		switch {
		case errors.Is(err, ErrPassphraseIncorrect) && (explicit == nil || hasSecrets):
			out.Identity = SecretsPassphraseIncorrect
		case err != nil:
			return SecretsOutcome{}, err
		default:
			out.Identity = SecretsOpened
			out.identity = identity
		}
	}
	if out.secrets != nil || out.identity != nil {
		out.passphrase = passphrase
		out.fromRequest = explicit != nil
	}
	return done()
}

// archiveStackNames lists, sorted, the stacks the archive's manifest holds
// files of. It only names them: a path RestoreFiles would refuse is refused
// there, and no name from here is ever used as a path.
func archiveStackNames(tree string) ([]string, error) {
	manifest, err := readManifest(filepath.Join(tree, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	var names []string
	for key := range manifest.Checksums {
		rel, ok := strings.CutPrefix(key, "stacks/")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rel, "/")
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// The reasons a stack .env is not restored.
const (
	NotRestoredNoSecrets           = "no_secrets"
	NotRestoredNoPassphrase        = "no_passphrase"
	NotRestoredPassphraseIncorrect = "passphrase_incorrect"
	NotRestoredStackNotInArchive   = "stack_not_in_archive"
	NotRestoredLeftInPlace         = "left_in_place"
)

// NotRestoredStackEnv is the Kind of a NotRestored that is a stack's .env.
const NotRestoredStackEnv = "stack_env"

// NotRestored is one thing an import did not restore, and why.
type NotRestored struct {
	Kind    string
	Name    string
	Reason  string
	Message string
}

// StackEnvsNotRestored lists the stack .env files an import with this
// outcome does not restore: those of the archive's stacks when the secrets
// section was not opened, those in the section for a stack the archive does
// not hold, and each .env under paths.StacksDir that nothing replaces, which
// stays as it is. It reads the directory and writes nothing.
func StackEnvsNotRestored(o SecretsOutcome, paths Paths) ([]NotRestored, error) {
	var out []NotRestored
	listed := map[string]bool{}
	if o.Status != SecretsOpened {
		reason, why := NotRestoredNoSecrets, "the archive has no secrets section"
		switch o.Status {
		case SecretsNoPassphrase:
			reason, why = NotRestoredNoPassphrase, "no backup passphrase is available to open the archive's secrets"
		case SecretsPassphraseIncorrect:
			reason, why = NotRestoredPassphraseIncorrect, "the configured backup passphrase does not open the archive's secrets"
		}
		for _, stack := range o.archiveStacks {
			listed[stack] = true
			out = append(out, NotRestored{Kind: NotRestoredStackEnv, Name: stack, Reason: reason,
				Message: fmt.Sprintf("the .env file of stack %s is not restored: %s", stack, why)})
		}
	}

	restored := map[string]bool{}
	var notInArchive []string
	for _, e := range o.StackEnvs() {
		if slices.Contains(o.archiveStacks, e.Stack) {
			restored[e.Stack] = true
		} else {
			notInArchive = append(notInArchive, e.Stack)
		}
	}
	for _, stack := range notInArchive {
		out = append(out, NotRestored{Kind: NotRestoredStackEnv, Name: stack, Reason: NotRestoredStackNotInArchive,
			Message: fmt.Sprintf("the archive's secrets hold an .env file for stack %s, which the archive has no files of", stack)})
	}

	live, err := liveStackEnvNames(paths.StacksDir)
	if err != nil {
		return nil, err
	}
	for _, stack := range live {
		if restored[stack] || listed[stack] {
			continue
		}
		out = append(out, NotRestored{Kind: NotRestoredStackEnv, Name: stack, Reason: NotRestoredLeftInPlace,
			Message: fmt.Sprintf("the .env file of stack %s is left as it is: the archive holds none for it", stack)})
	}
	return out, nil
}

// liveStackEnvNames lists, sorted, the stacks under dir that have an .env.
func liveStackEnvNames(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		_, err := os.Lstat(filepath.Join(dir, e.Name(), ".env"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		names = append(names, e.Name())
	}
	return names, nil
}
