package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	// ErrInvalidDestination wraps every reason PrepareDestination refuses
	// a new destination's own fields.
	ErrInvalidDestination = errors.New("backup: invalid destination")
	// ErrDestinationNotFound is a destination id no row has.
	ErrDestinationNotFound = errors.New("backup: no such destination")
	// ErrDestinationExists is a new destination whose name, or whose
	// target, an existing destination already has.
	ErrDestinationExists = errors.New("backup: a destination with that name or target already exists")
)

// StaleAfter is how long an enabled destination may go without a
// successful backup before it is stale. The nightly chain (Q30) runs once
// a day, so two days is one missed night plus the slack of a slow one.
const StaleAfter = 48 * time.Hour

// DestinationStore persists destinations (D4). It has no business logic:
// Service owns validation, sealing and staleness.
type DestinationStore interface {
	// ListDestinations returns every destination, oldest first.
	ListDestinations(ctx context.Context) ([]Destination, error)
	// GetDestination returns ErrDestinationNotFound for an unknown id.
	GetDestination(ctx context.Context, id string) (Destination, error)
	CreateDestination(ctx context.Context, d Destination) error
	// SeedDestinations stores every destination in ds, or none of them,
	// and only while no destination exists at all; it does nothing
	// otherwise.
	SeedDestinations(ctx context.Context, ds []Destination) error
	// DeleteDestination returns ErrDestinationNotFound for an unknown id.
	// Deleting "external:<label>" also clears that disk's backup-destination
	// flag, in the same transaction (ExternalDestinationStore).
	DeleteDestination(ctx context.Context, id string) error
	// RecordBackupSuccess sets the last successful backup time and clears
	// the stale-alert marker, so a later staleness alerts again.
	RecordBackupSuccess(ctx context.Context, id string, at time.Time) error
	// MarkStaleAlerted sets the stale-alert marker only while the row's
	// last successful backup time still equals observed, so a success
	// recorded after the stale check read the row is never masked. It
	// returns ErrDestinationNotFound when the id is unknown or the time
	// has moved on.
	MarkStaleAlerted(ctx context.Context, id string, observed *time.Time, at time.Time) error
	// UpdateDestination changes only the fields u sets, in one
	// transaction, and returns ErrDestinationNotFound for an unknown id.
	// Switching a disabled destination on records at as its EnabledAt and
	// clears its stale-alert marker, so the staleness it was paused with
	// is not carried over; any other update leaves both alone.
	UpdateDestination(ctx context.Context, id string, u DestinationUpdate, at time.Time) error
}

// DestinationUpdate changes a destination in place. A nil field is left
// as it is; type, path, options and credentials are never changed (remove
// and re-add).
type DestinationUpdate struct {
	Enabled   *bool
	Retention *Retention
}

// backendSpec describes what one remote destination type accepts, in
// rclone's own option names. Anything outside these lists is refused, so
// a request can never inject an arbitrary rclone option.
type backendSpec struct {
	rclone          string
	required        []string
	optional        []string
	secrets         []string
	secretsRequired []string
	// needOne lists options and secrets of which at least one must be set.
	needOne []string
	// obscured secrets are passed to rclone in its obscured form, which
	// its password options require.
	obscured     []string
	pathRequired bool
}

var backendSpecs = map[DestinationType]backendSpec{
	TypeSMB: {
		rclone: "smb", required: []string{"host", "user"}, optional: []string{"port", "domain"},
		secrets: []string{"pass"}, secretsRequired: []string{"pass"}, obscured: []string{"pass"}, pathRequired: true,
	},
	TypeS3: {
		rclone: "s3", required: []string{"access_key_id"}, optional: []string{"provider", "endpoint", "region"},
		secrets: []string{"secret_access_key"}, secretsRequired: []string{"secret_access_key"}, pathRequired: true,
	},
	TypeSFTP: {
		rclone: "sftp", required: []string{"host", "user"}, optional: []string{"port", "key_file", "known_hosts_file"},
		secrets: []string{"pass"}, needOne: []string{"pass", "key_file"}, obscured: []string{"pass"},
	},
	TypeWebDAV: {
		rclone: "webdav", required: []string{"url", "user"}, optional: []string{"vendor"},
		secrets: []string{"pass"}, secretsRequired: []string{"pass"}, obscured: []string{"pass"},
	},
	TypeRclone: {required: []string{"remote"}},
}

var rcloneRemoteNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// NewDestination is a request to add a destination. Nil Enabled, Encrypt
// and Retention take their defaults.
type NewDestination struct {
	Name      string
	Type      DestinationType
	Path      string
	Options   map[string]string
	Secrets   map[string]string
	Enabled   *bool
	Encrypt   *bool
	Retention *Retention
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidDestination, fmt.Sprintf(format, args...))
}

// ValidateRetention is the bound a destination's retention must meet, when
// it is created and when it is changed.
func ValidateRetention(r Retention) error {
	if r.Daily < 0 || r.Weekly < 0 || r.Monthly < 0 || r.Daily > 1000 || r.Weekly > 1000 || r.Monthly > 1000 {
		return invalidf("retention counts must be between 0 and 1000")
	}
	if r.Daily+r.Weekly+r.Monthly == 0 {
		return invalidf("retention must keep at least one archive")
	}
	return nil
}

func cleanValue(field, v string) error {
	if len(v) > 1024 {
		return invalidf("%s is too long", field)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return invalidf("%s contains a control character", field)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// PrepareDestination validates nd against the destinations that already
// exist and returns the Destination to store, without an ID or creation
// time. It is the one admission check both the daemon and cmd/mockapi run
// (D18). Remote destinations always encrypt (Q80), and are refused
// through ValidateRemoteDestination while no backup passphrase is set.
//
// protected lists directories a local destination may not be, or lie under,
// beyond the system locations refused always: the daemon passes its state
// and config directories.
func PrepareDestination(nd NewDestination, existing []Destination, hasPassphrase bool, protected ...string) (Destination, error) {
	name := strings.TrimSpace(nd.Name)
	if name == "" || len(name) > 128 {
		return Destination{}, invalidf("name must be 1 to 128 characters")
	}
	if err := cleanValue("name", name); err != nil {
		return Destination{}, err
	}

	dest := Destination{Name: name, Type: nd.Type, Enabled: true, Retention: Retention{
		Daily: DefaultRetentionDaily, Weekly: DefaultRetentionWeekly, Monthly: DefaultRetentionMonthly,
	}}
	if nd.Enabled != nil {
		dest.Enabled = *nd.Enabled
	}
	if nd.Retention != nil {
		if err := ValidateRetention(*nd.Retention); err != nil {
			return Destination{}, err
		}
		dest.Retention = *nd.Retention
	}

	switch nd.Type {
	case TypeLocal:
		if len(nd.Options) > 0 || len(nd.Secrets) > 0 {
			return Destination{}, invalidf("a local destination takes no options or secrets")
		}
		if err := cleanValue("path", nd.Path); err != nil {
			return Destination{}, err
		}
		if !filepath.IsAbs(nd.Path) {
			return Destination{}, invalidf("a local destination's path must be absolute")
		}
		clean := filepath.Clean(nd.Path)
		if clean == string(filepath.Separator) {
			return Destination{}, invalidf("a local destination cannot be the filesystem root")
		}
		if err := refuseSystemPath(clean, protected); err != nil {
			return Destination{}, err
		}
		dest.Path = clean
		dest.Type = TypeLocal
		dest.Encrypt = nd.Encrypt != nil && *nd.Encrypt
	case TypeSMB, TypeS3, TypeSFTP, TypeWebDAV, TypeRclone:
		spec := backendSpecs[nd.Type]
		if nd.Encrypt != nil && !*nd.Encrypt {
			return Destination{}, invalidf("a remote destination is always encrypted")
		}
		if err := ValidateRemoteDestination(hasPassphrase); err != nil {
			return Destination{}, err
		}
		if err := cleanValue("path", nd.Path); err != nil {
			return Destination{}, err
		}
		for _, seg := range strings.Split(nd.Path, "/") {
			if seg == ".." {
				return Destination{}, invalidf("path must not contain '..'")
			}
		}
		if spec.pathRequired && strings.Trim(nd.Path, "/") == "" {
			return Destination{}, invalidf("a %s destination needs a path (%s)", nd.Type, pathHint(nd.Type))
		}
		if err := checkKeys("option", nd.Options, append(append([]string{}, spec.required...), spec.optional...)); err != nil {
			return Destination{}, err
		}
		if err := checkKeys("secret", nd.Secrets, spec.secrets); err != nil {
			return Destination{}, err
		}
		for _, k := range spec.required {
			if nd.Options[k] == "" {
				return Destination{}, invalidf("option %q is required for a %s destination", k, nd.Type)
			}
		}
		for _, k := range spec.secretsRequired {
			if nd.Secrets[k] == "" {
				return Destination{}, invalidf("secret %q is required for a %s destination", k, nd.Type)
			}
		}
		if len(spec.needOne) > 0 {
			found := false
			for _, k := range spec.needOne {
				if nd.Options[k] != "" || nd.Secrets[k] != "" {
					found = true
				}
			}
			if !found {
				return Destination{}, invalidf("a %s destination needs one of %s", nd.Type, strings.Join(spec.needOne, " or "))
			}
		}
		if nd.Type == TypeRclone && !rcloneRemoteNamePattern.MatchString(nd.Options["remote"]) {
			return Destination{}, invalidf("option \"remote\" must be the name of a configured rclone remote")
		}
		dest.Path = nd.Path
		dest.Encrypt = true
		dest.Options = copyMap(nd.Options)
		dest.Secrets = copyMap(nd.Secrets)
	default:
		return Destination{}, invalidf("type must be one of local, smb, s3, sftp, webdav or rclone")
	}

	for _, e := range existing {
		if strings.EqualFold(e.Name, dest.Name) {
			return Destination{}, ErrDestinationExists
		}
		if sameTarget(e, dest) {
			return Destination{}, ErrDestinationExists
		}
	}
	return dest, nil
}

// systemTrees are directories no backup destination may be or lie under.
var systemTrees = []string{"/etc", "/usr", "/boot", "/proc", "/sys", "/dev", "/run"}

// systemDirs are directories that may not themselves be a destination but
// hold ordinary ones below them (a temporary directory, /var/lib/...).
var systemDirs = []string{"/tmp", "/var"}

// refuseSystemPath refuses a local destination in a system location or in
// the daemon's own state or configuration, where an archive directory
// would sit among files that are not backups. Symlinks are resolved, so a
// link into one of those places is refused too. The state directory's own
// backups subdirectory, the boot default's home, stays allowed.
func refuseSystemPath(path string, protected []string) error {
	for _, p := range []string{path, resolveExisting(path)} {
		for _, tree := range systemTrees {
			if withinDir(p, tree) {
				return invalidf("a local destination cannot be %s or inside it", tree)
			}
		}
		for _, dir := range systemDirs {
			if p == dir {
				return invalidf("a local destination cannot be %s itself; use a directory inside it", dir)
			}
		}
		for _, dir := range protected {
			if dir == "" {
				continue
			}
			dir = filepath.Clean(dir)
			if withinDir(p, dir) && !withinDir(p, filepath.Join(dir, "backups")) {
				return invalidf("a local destination cannot be Hoserva's own state or configuration directory %s or inside it", dir)
			}
		}
	}
	return nil
}

// withinDir reports whether path is dir or lies under it, by path
// component.
func withinDir(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// resolveExisting resolves symlinks in the longest existing prefix of
// path, so a not-yet-created destination below a link is judged by where
// it would really be written.
func resolveExisting(path string) string {
	rest := ""
	for p := path; ; p = filepath.Dir(p) {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		if p == filepath.Dir(p) {
			return path
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

func pathHint(t DestinationType) string {
	if t == TypeSMB {
		return "the share name and folder"
	}
	return "the bucket and prefix"
}

func checkKeys(kind string, m map[string]string, allowed []string) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !contains(allowed, k) {
			return invalidf("%s %q is not accepted for this destination type", kind, k)
		}
		if err := cleanValue(kind+" "+k, m[k]); err != nil {
			return err
		}
	}
	return nil
}

func copyMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sameTarget(a, b Destination) bool {
	at, bt := a.Type, b.Type
	if at == "" {
		at = TypeLocal
	}
	if bt == "" {
		bt = TypeLocal
	}
	if at != bt || a.Path != b.Path || len(a.Options) != len(b.Options) {
		return false
	}
	for k, v := range a.Options {
		if b.Options[k] != v {
			return false
		}
	}
	return true
}

// IsStale reports whether an enabled destination has gone StaleAfter
// without a successful backup, counted from its creation until its first
// one, or from the last time it was switched on if that is later. It reads
// only stored timestamps — it never looks at the destination itself.
func IsStale(d Destination, now time.Time) bool {
	if !d.Enabled {
		return false
	}
	ref := d.CreatedAt
	if d.LastSuccessfulBackupAt != nil {
		ref = *d.LastSuccessfulBackupAt
	}
	if d.EnabledAt != nil && d.EnabledAt.After(ref) {
		ref = *d.EnabledAt
	}
	return now.Sub(ref) > StaleAfter
}

func newDestinationID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating destination id: %w", err)
	}
	return "dest-" + hex.EncodeToString(b[:]), nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) rcloneRunner() RcloneRunner {
	if s.Rclone != nil {
		return s.Rclone
	}
	return ExecRclone{}
}

// runRclone runs a short rclone call under its own deadline.
func (s *Service) runRclone(ctx context.Context, c RcloneCommand) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, rcloneQuickTimeout)
	defer cancel()
	return s.rcloneRunner().Run(ctx, c)
}

func (s *Service) hasPassphrase(ctx context.Context) (bool, error) {
	if s.Secrets == nil {
		return false, nil
	}
	p, ok, err := s.Secrets.BackupPassphrase(ctx)
	if err != nil {
		return false, fmt.Errorf("reading backup passphrase: %w", err)
	}
	return ok && p != "", nil
}

func (s *Service) requireStore() (DestinationStore, error) {
	if s.Store == nil {
		return nil, errors.New("backup: destinations are not configured")
	}
	return s.Store, nil
}

// ListDestinations returns every destination with its credentials still
// sealed.
func (s *Service) ListDestinations(ctx context.Context) ([]Destination, error) {
	st, err := s.requireStore()
	if err != nil {
		return nil, err
	}
	return st.ListDestinations(ctx)
}

// SeedDestinations stores defaults only while no destination exists at
// all, so a fresh install (or one upgraded from before destinations were
// stored) starts with Q40's two local destinations and a deliberate
// removal is not undone by a restart while any destination remains. The
// store writes all of them or none, so a failure part way leaves the table
// empty and the next start seeds every default.
func (s *Service) SeedDestinations(ctx context.Context, defaults []Destination) error {
	st, err := s.requireStore()
	if err != nil {
		return err
	}
	s.destMu.Lock()
	defer s.destMu.Unlock()
	now := s.now()
	seeded := make([]Destination, len(defaults))
	for i, d := range defaults {
		if d.CreatedAt.IsZero() {
			d.CreatedAt = now
		}
		seeded[i] = d
	}
	if err := st.SeedDestinations(ctx, seeded); err != nil {
		return fmt.Errorf("storing default destinations: %w", err)
	}
	return nil
}

// AddDestination validates and stores a new destination. A remote one
// needs rclone installed: without it the call fails with ErrRcloneMissing
// carrying the install command, before anything is stored.
func (s *Service) AddDestination(ctx context.Context, nd NewDestination) (Destination, error) {
	st, err := s.requireStore()
	if err != nil {
		return Destination{}, err
	}
	hasPass, err := s.hasPassphrase(ctx)
	if err != nil {
		return Destination{}, err
	}

	s.destMu.Lock()
	defer s.destMu.Unlock()
	existing, err := st.ListDestinations(ctx)
	if err != nil {
		return Destination{}, fmt.Errorf("listing destinations: %w", err)
	}
	dest, err := PrepareDestination(nd, existing, hasPass, s.Paths.StateDir, s.Paths.ConfigRoot)
	if err != nil {
		return Destination{}, err
	}
	if dest.isRemote() {
		if _, err := s.runRclone(ctx, RcloneCommand{Args: []string{"version"}}); err != nil {
			if errors.Is(err, ErrRcloneMissing) {
				return Destination{}, err
			}
			return Destination{}, fmt.Errorf("checking rclone: %w", err)
		}
	}
	if len(dest.Secrets) > 0 {
		if s.DestinationCipher == nil {
			return Destination{}, errors.New("backup: no cipher configured to seal destination credentials")
		}
		raw, err := json.Marshal(dest.Secrets)
		if err != nil {
			return Destination{}, fmt.Errorf("encoding destination credentials: %w", err)
		}
		sealed, err := s.DestinationCipher.Encrypt(raw)
		if err != nil {
			return Destination{}, fmt.Errorf("sealing destination credentials: %w", err)
		}
		dest.SealedSecrets = sealed
		dest.Secrets = nil
	}
	if dest.ID, err = newDestinationID(); err != nil {
		return Destination{}, err
	}
	dest.CreatedAt = s.now()
	if err := st.CreateDestination(ctx, dest); err != nil {
		return Destination{}, fmt.Errorf("storing destination: %w", err)
	}
	return dest, nil
}

// RemoveDestination deletes the destination's configuration. Archives
// already written to it are left where they are.
func (s *Service) RemoveDestination(ctx context.Context, id string) error {
	st, err := s.requireStore()
	if err != nil {
		return err
	}
	s.destMu.Lock()
	defer s.destMu.Unlock()
	return st.DeleteDestination(ctx, id)
}

// UpdateDestination changes a destination's enabled flag and retention in
// place, validating the retention as AddDestination does before anything
// is written. Disabling never removes an archive or the credentials, and
// the next run reads the new values from the store. An update that sets
// neither field changes nothing and returns the destination as it is.
func (s *Service) UpdateDestination(ctx context.Context, id string, u DestinationUpdate) (Destination, error) {
	st, err := s.requireStore()
	if err != nil {
		return Destination{}, err
	}
	if u.Retention != nil {
		if err := ValidateRetention(*u.Retention); err != nil {
			return Destination{}, err
		}
	}
	s.destMu.Lock()
	defer s.destMu.Unlock()
	if u.Enabled != nil || u.Retention != nil {
		if err := st.UpdateDestination(ctx, id, u, s.now()); err != nil {
			return Destination{}, err
		}
	}
	return st.GetDestination(ctx, id)
}

// StaleAlert publishes one stale-destination alert.
type StaleAlert func(ctx context.Context, name string, lastSuccessfulBackupAt *time.Time) error

// CheckStaleDestinations publishes alert once for each enabled destination
// that has gone StaleAfter without a successful backup, and not again
// until a backup to it succeeds. It reads stored timestamps only; it never
// lists a destination's contents. A destination whose alert fails to send
// is retried on the next call.
func (s *Service) CheckStaleDestinations(ctx context.Context, now time.Time, alert StaleAlert) error {
	st, err := s.requireStore()
	if err != nil {
		return err
	}
	dests, err := st.ListDestinations(ctx)
	if err != nil {
		return fmt.Errorf("listing destinations: %w", err)
	}
	var failures []error
	for _, d := range dests {
		if !IsStale(d, now) || d.StaleAlertedAt != nil {
			continue
		}
		if err := alert(ctx, d.Name, d.LastSuccessfulBackupAt); err != nil {
			failures = append(failures, fmt.Errorf("alerting on stale destination %q: %w", d.ID, err))
			continue
		}
		err := st.MarkStaleAlerted(ctx, d.ID, d.LastSuccessfulBackupAt, now)
		if err != nil && !errors.Is(err, ErrDestinationNotFound) {
			failures = append(failures, fmt.Errorf("marking destination %q alerted: %w", d.ID, err))
		}
	}
	return errors.Join(failures...)
}

// TestResult is the outcome of a destination connection test.
type TestResult struct {
	Success bool
	Error   string
}

// TestDestination writes a small file to the destination, reads it back
// and deletes it. A destination that could not be reached, or that
// returned different bytes, is a result with Success false; a missing
// rclone or an unknown id is an error. The file is random and carries no
// archive content, so it is not encrypted.
func (s *Service) TestDestination(ctx context.Context, id string) (TestResult, error) {
	st, err := s.requireStore()
	if err != nil {
		return TestResult{}, err
	}
	dest, err := st.GetDestination(ctx, id)
	if err != nil {
		return TestResult{}, err
	}
	release, refusal := s.admitDestination(ctx, dest)
	if refusal != "" {
		return TestResult{Error: refusal}, nil
	}
	defer release()

	target, err := s.targetFor(ctx, dest)
	if err != nil {
		return failedTest(err)
	}

	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return TestResult{}, fmt.Errorf("generating test content: %w", err)
	}
	probe, err := os.CreateTemp("", "hoserva-connection-test-*")
	if err != nil {
		return TestResult{}, fmt.Errorf("creating test file: %w", err)
	}
	defer func() { _ = os.Remove(probe.Name()) }()
	_, werr := probe.Write(token)
	if cerr := probe.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return TestResult{}, fmt.Errorf("writing test file: %w", werr)
	}
	name := filepath.Base(probe.Name())

	writeErr := target.write(ctx, probe.Name())
	var readErr error
	if writeErr == nil {
		var got []byte
		if got, readErr = target.readBack(ctx, name); readErr == nil && string(got) != string(token) {
			readErr = errors.New("the file read back differs from the one written")
		}
	}
	// The cleanup outlives a cancelled request: a test file left on the
	// destination is worse than one more bounded call.
	removeErr := target.remove(context.WithoutCancel(ctx), name)

	for _, err := range []error{writeErr, readErr, removeErr} {
		if err != nil {
			return failedTest(err)
		}
	}
	return TestResult{Success: true}, nil
}

func failedTest(err error) (TestResult, error) {
	if errors.Is(err, ErrRcloneMissing) {
		return TestResult{}, err
	}
	return TestResult{Error: err.Error()}, nil
}

// targetFor builds the archiveTarget for dest, unsealing a remote
// destination's credentials for the duration of one operation.
func (s *Service) targetFor(ctx context.Context, dest Destination) (archiveTarget, error) {
	if !dest.isRemote() {
		return localTarget{dest: dest}, nil
	}
	if dest.Type == TypeRclone {
		return &rcloneTarget{runner: s.rcloneRunner(), dir: dest.Options["remote"] + ":" + dest.Path}, nil
	}
	spec, ok := backendSpecs[dest.Type]
	if !ok {
		return nil, fmt.Errorf("unknown destination type %q", dest.Type)
	}
	secrets := dest.Secrets
	if secrets == nil && len(dest.SealedSecrets) > 0 {
		if s.Cipher == nil {
			return nil, errors.New("no cipher configured to unseal destination credentials")
		}
		raw, err := s.Cipher.Decrypt(dest.SealedSecrets)
		if err != nil {
			return nil, fmt.Errorf("unsealing destination credentials: %w", err)
		}
		if err := json.Unmarshal(raw, &secrets); err != nil {
			return nil, fmt.Errorf("decoding destination credentials: %w", err)
		}
	}

	prefix := "RCLONE_CONFIG_" + rcloneRemoteName + "_"
	env := []string{prefix + "TYPE=" + spec.rclone}
	for _, k := range sortedMapKeys(dest.Options) {
		env = append(env, prefix+strings.ToUpper(k)+"="+dest.Options[k])
	}
	for _, k := range sortedMapKeys(secrets) {
		v := secrets[k]
		if contains(spec.obscured, k) {
			out, err := s.runRclone(ctx, RcloneCommand{Args: []string{"obscure", "-"}, Stdin: []byte(v)})
			if err != nil {
				return nil, fmt.Errorf("preparing credentials for rclone: %w", err)
			}
			v = strings.TrimSpace(string(out))
		}
		env = append(env, prefix+strings.ToUpper(k)+"="+v)
	}
	return &rcloneTarget{runner: s.rcloneRunner(), env: env, dir: rcloneRemoteName + ":" + dest.Path}, nil
}

func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
