package backup

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// packArchive writes dir's contents as a tar.zst stream to dest.
func packArchive(dir, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("creating archive parent directory: %w", err)
	}

	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating archive temp file: %w", err)
	}

	zw, err := zstd.NewWriter(out)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("creating zstd writer: %w", err)
	}
	tw := tar.NewWriter(zw)

	if err := addDirToTar(tw, dir, ""); err != nil {
		_ = tw.Close()
		_ = zw.Close()
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("closing tar writer: %w", err)
	}
	if err := zw.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("closing zstd writer: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("syncing archive temp file: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("closing archive temp file: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("finalizing archive: %w", err)
	}
	return fsyncDir(filepath.Dir(dest))
}

func addDirToTar(tw *tar.Writer, root, prefix string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(filepath.Join(prefix, rel))

		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		if _, err := io.Copy(tw, f); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	})
}

// unpackArchive extracts archivePath into dest.
func unpackArchive(archivePath, dest string) error {
	in, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening archive: %w", err)
	}
	defer func() { _ = in.Close() }()

	zr, err := zstd.NewReader(in)
	if err != nil {
		return fmt.Errorf("creating zstd reader: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}
		target := filepath.Join(dest, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) && filepath.Clean(target) != filepath.Clean(dest) {
			return fmt.Errorf("archive entry %q escapes destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			continue
		}
	}
	return nil
}
