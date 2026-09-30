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
)

var (
	// ErrNoSecrets: the archive has no secrets.age, which is what an archive
	// built without a backup passphrase looks like.
	ErrNoSecrets = errors.New("the archive has no secrets.age")
	// ErrPassphraseIncorrect: the passphrase does not open secrets.age.
	ErrPassphraseIncorrect = errors.New("the passphrase does not open secrets.age")
	// ErrSecretsUnreadable: secrets.age is not a readable age file, or its
	// content is not what BuildArchive writes.
	ErrSecretsUnreadable = errors.New("secrets.age cannot be read")
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
// can be restored. Stacks lists, when it cannot, the stacks of the archive
// whose .env files would not be restored.
type SecretsOutcome struct {
	Status string
	Stacks []string

	secrets       *Secrets
	archiveStacks []string
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
// secrets.age. A passphrase given explicitly is the only one tried, and one
// that does not open the file is ErrPassphraseIncorrect; without one the
// configured backup passphrase (src) is tried, and if that is absent or does
// not open the file the outcome says so, so that everything else can still
// be restored. An explicit passphrase for an archive with no secrets.age is
// not an error: there is nothing to check it against. Nothing is written.
func ResolveSecrets(ctx context.Context, tree string, src SecretSource, explicit *string) (SecretsOutcome, error) {
	archiveStacks, err := archiveStackNames(tree)
	if err != nil {
		return SecretsOutcome{}, err
	}
	out := SecretsOutcome{archiveStacks: archiveStacks}
	notOpened := func(status string) (SecretsOutcome, error) {
		out.Status = status
		out.Stacks = slices.Clone(archiveStacks)
		return out, nil
	}

	if _, err := os.Lstat(filepath.Join(tree, "secrets.age")); errors.Is(err, fs.ErrNotExist) {
		return notOpened(SecretsNone)
	} else if err != nil {
		return SecretsOutcome{}, fmt.Errorf("checking for secrets.age: %w", err)
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
		if !ok || p == "" {
			return notOpened(SecretsNoPassphrase)
		}
		passphrase = p
	default:
		return notOpened(SecretsNoPassphrase)
	}

	secrets, err := ReadSecrets(tree, passphrase)
	switch {
	case errors.Is(err, ErrPassphraseIncorrect) && explicit == nil:
		return notOpened(SecretsPassphraseIncorrect)
	case err != nil:
		return SecretsOutcome{}, err
	}
	out.Status = SecretsOpened
	out.secrets = secrets
	return out, nil
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
