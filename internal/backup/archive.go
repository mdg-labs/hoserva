package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Paths names every on-disk source the archive builder reads. Callers in
// tests and dev runs point these at temp directories — never /etc/hoserva
// or /var/lib/hoserva directly (CLAUDE.md).
type Paths struct {
	StateDir            string
	ConfigRoot          string
	DBPath              string
	StacksDir           string
	TemplatesDir        string
	SnapraidContentPath string
}

// BuildArchive assembles doc 10 §1's layout in stagingDir and returns the
// manifest checksum map. stagingDir must already exist.
func BuildArchive(ctx context.Context, db *sql.DB, paths Paths, src SecretSource, cipher SecretCipher, host, version string, now time.Time, stagingDir string) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}

	stateDB := filepath.Join(stagingDir, "state.db")
	if err := vacuumInto(ctx, db, stateDB); err != nil {
		return Manifest{}, fmt.Errorf("snapshotting database: %w", err)
	}

	stackEnvs, err := collectStackEnvs(paths.StacksDir)
	if err != nil {
		return Manifest{}, err
	}
	if err := copyStacksComposeOnly(paths.StacksDir, filepath.Join(stagingDir, "stacks")); err != nil {
		return Manifest{}, err
	}

	secrets, err := buildSecretsAge(ctx, src, cipher, stackEnvs)
	if err != nil {
		return Manifest{}, err
	}
	if len(secrets) > 0 {
		if err := os.WriteFile(filepath.Join(stagingDir, "secrets.age"), secrets, 0o600); err != nil {
			return Manifest{}, fmt.Errorf("writing secrets.age: %w", err)
		}
	}

	if err := copyTreeIfExists(paths.ConfigRoot, filepath.Join(stagingDir, "generated"), isGeneratedFile); err != nil {
		return Manifest{}, err
	}
	if err := copyTreeIfExists(paths.ConfigRoot, filepath.Join(stagingDir, "custom"), isCustomFile); err != nil {
		return Manifest{}, err
	}
	if err := copyTreeIfExists(paths.TemplatesDir, filepath.Join(stagingDir, "templates"), nil); err != nil {
		return Manifest{}, err
	}
	if err := copySnapraidContent(paths.SnapraidContentPath, filepath.Join(stagingDir, "snapraid-content")); err != nil {
		return Manifest{}, err
	}

	checksums := map[string]string{}
	files, err := listArchiveFiles(stagingDir)
	if err != nil {
		return Manifest{}, err
	}
	for _, rel := range files {
		sum, err := hashFile(filepath.Join(stagingDir, rel))
		if err != nil {
			return Manifest{}, fmt.Errorf("hashing %s: %w", rel, err)
		}
		checksums[rel] = sum
	}

	manifest := buildManifest(host, version, now, checksums)
	if err := writeManifest(filepath.Join(stagingDir, "manifest.json"), manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func isGeneratedFile(rel string) bool {
	base := filepath.Base(rel)
	if strings.HasPrefix(base, ".") {
		return false
	}
	if base == "smb.custom.conf" || strings.HasSuffix(base, ".custom.conf") {
		return false
	}
	return true
}

func isCustomFile(rel string) bool {
	base := filepath.Base(rel)
	return strings.HasSuffix(base, ".custom.conf")
}

func listArchiveFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "manifest.json" {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out, err
}

func copyTreeIfExists(src, dst string, include func(rel string) bool) error {
	if src == "" {
		return nil
	}
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("resolving source path %q: %w", src, err)
	}
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return fmt.Errorf("resolving destination path %q: %w", dst, err)
	}
	if absSrc == absDst || strings.HasPrefix(absDst, absSrc+string(os.PathSeparator)) {
		return fmt.Errorf("copy destination %q is inside source %q", dst, src)
	}
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checking %s: %w", src, err)
	}
	if !info.IsDir() {
		if include != nil && !include(filepath.Base(src)) {
			return nil
		}
		return copyPath(filepath.Join(dst, filepath.Base(src)), src)
	}

	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if include != nil && !include(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		return copyPath(target, path)
	})
}

func copySnapraidContent(src, dstDir string) error {
	if src == "" {
		return nil
	}
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checking snapraid content %s: %w", src, err)
	}
	if info.IsDir() {
		return copyTreeIfExists(src, dstDir, nil)
	}
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return err
	}
	return copyPath(filepath.Join(dstDir, filepath.Base(src)), src)
}

func collectStackEnvs(stacksDir string) ([]StackEnv, error) {
	var out []StackEnv
	info, err := os.Stat(stacksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("checking stacks directory: %w", err)
	}
	if !info.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(stacksDir)
	if err != nil {
		return nil, fmt.Errorf("listing stacks: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		envPath := filepath.Join(stacksDir, e.Name(), ".env")
		body, err := os.ReadFile(envPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("reading %s: %w", envPath, err)
		}
		out = append(out, StackEnv{Stack: e.Name(), Body: body})
	}
	return out, nil
}

func copyStacksComposeOnly(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checking stacks directory: %w", err)
	}
	if !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("listing stacks: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		stackSrc := filepath.Join(src, e.Name())
		stackDst := filepath.Join(dst, e.Name())
		if err := os.MkdirAll(stackDst, 0o700); err != nil {
			return err
		}
		for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml", "meta.json"} {
			srcFile := filepath.Join(stackSrc, name)
			if _, err := os.Stat(srcFile); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return err
			}
			if err := copyPath(filepath.Join(stackDst, name), srcFile); err != nil {
				return fmt.Errorf("copying %s: %w", name, err)
			}
		}
	}
	return nil
}
