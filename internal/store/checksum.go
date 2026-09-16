package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ChecksumOf hashes a migration's exact file content. Any later change to
// that content — including whitespace — changes this hash, which is
// exactly what "migrations are immutable once created" (D16) needs to be
// enforceable rather than aspirational.
func ChecksumOf(m Migration) string {
	sum := sha256.Sum256([]byte(m.SQL))
	return hex.EncodeToString(sum[:])
}

// ReadChecksums parses the "checksums" file format: one line per migration,
// "<sha256>  <filename>", sorted by filename. It is itself plain text
// tracked in git, so an edited migration shows up as a diff in the
// checksums file too, not just a mismatch discovered at check time.
func ReadChecksums(data []byte) (map[string]string, error) {
	out := map[string]string{}
	for lineNum, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("checksums line %d: expected \"<sha256>  <filename>\", got %q", lineNum+1, line)
		}
		out[fields[1]] = fields[0]
	}
	return out, nil
}

// FormatChecksums renders the checksums map back to the file format above,
// sorted by filename so the generated file's diff is stable.
func FormatChecksums(sums map[string]string) []byte {
	names := make([]string, 0, len(sums))
	for name := range sums {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s  %s\n", sums[name], name)
	}
	return []byte(b.String())
}

// VerifyChecksums fails if any migration's current content no longer
// matches its recorded checksum (an edited "immutable" file), if a
// migration file has no recorded checksum (never registered), or if a
// checksum is recorded for a file that no longer exists.
func VerifyChecksums(migrations []Migration, recorded map[string]string) error {
	seen := map[string]bool{}
	for _, m := range migrations {
		seen[m.Filename] = true
		want, ok := recorded[m.Filename]
		if !ok {
			return fmt.Errorf("migration %q has no recorded checksum — run `make db-migration` to regenerate the checksums file", m.Filename)
		}
		got := ChecksumOf(m)
		if got != want {
			return fmt.Errorf("migration %q has changed since it was generated (checksum %s, recorded %s) — migrations are immutable once created (D16); fix schema.sql and generate a new migration instead", m.Filename, got, want)
		}
	}
	for name := range recorded {
		if !seen[name] {
			return fmt.Errorf("checksums file records %q, which no longer exists under internal/store/migrations/", name)
		}
	}
	return nil
}

// AppendChecksum registers exactly one new migration's checksum in dir's
// checksums file, leaving every existing entry's recorded hash untouched.
// This is the only way `make db-migration` may update the checksums file:
// rehashing every file in dir, as if regenerating the file from scratch,
// would silently re-approve an edit to an existing
// "immutable" migration instead of catching it — the whole point of
// checksumming them at all. Callers verify every existing migration's
// checksum first (VerifyChecksums), before ever calling this.
func AppendChecksum(dir string, m Migration) error {
	data, err := os.ReadFile(filepath.Join(dir, ChecksumsFile))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading existing checksums: %w", err)
	}
	sums := map[string]string{}
	if err == nil {
		sums, err = ReadChecksums(data)
		if err != nil {
			return err
		}
	}
	if _, exists := sums[m.Filename]; exists {
		return fmt.Errorf("checksums file already records %q — refusing to overwrite an existing entry", m.Filename)
	}
	sums[m.Filename] = ChecksumOf(m)
	return os.WriteFile(filepath.Join(dir, ChecksumsFile), FormatChecksums(sums), 0o644)
}
