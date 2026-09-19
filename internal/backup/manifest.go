package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Manifest is manifest.json inside every archive (doc 10 §1).
type Manifest struct {
	Version    int               `json:"version"`
	Timestamp  time.Time         `json:"timestamp"`
	Host       string            `json:"host"`
	Hoserva    string            `json:"hoserva_version"`
	Checksums  map[string]string `json:"checksums"`
	ArchiveSHA string            `json:"archive_sha256,omitempty"`
}

func buildManifest(host, version string, now time.Time, files map[string]string) Manifest {
	return Manifest{
		Version:   ArchiveVersion,
		Timestamp: now.UTC(),
		Host:      host,
		Hoserva:   version,
		Checksums: files,
	}
}

func writeManifest(path string, m Manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding manifest: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}
	return nil
}

func readManifest(path string) (Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("reading manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("parsing manifest: %w", err)
	}
	return m, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := copyFile(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
