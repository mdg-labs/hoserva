package cache

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// copyXattrs copies every extended attribute from src to dst (doc 09 §2:
// "preserving mode, ownership, xattrs, ACLs, timestamps"). POSIX ACLs are
// themselves stored as the system.posix_acl_access / system.posix_acl_default
// xattrs, so copying every xattr also carries ACLs without a separate ACL
// library. A filesystem that does not support xattrs at all is not an
// error — there is nothing to preserve.
func copyXattrs(src, dst string) error {
	names, err := listXattrs(src)
	if err != nil {
		if isXattrUnsupported(err) {
			return nil
		}
		return fmt.Errorf("cache: list xattrs on %s: %w", src, err)
	}
	for _, name := range names {
		val, err := getXattr(src, name)
		if err != nil {
			if isXattrUnsupported(err) {
				continue
			}
			return fmt.Errorf("cache: read xattr %q on %s: %w", name, src, err)
		}
		if err := unix.Setxattr(dst, name, val, 0); err != nil {
			if isXattrUnsupported(err) {
				continue
			}
			return fmt.Errorf("cache: set xattr %q on %s: %w", name, dst, err)
		}
	}
	return nil
}

func listXattrs(path string) ([]string, error) {
	size, err := unix.Listxattr(path, nil)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	n, err := unix.Listxattr(path, buf)
	if err != nil {
		return nil, err
	}
	return splitXattrNames(buf[:n]), nil
}

func getXattr(path, name string) ([]byte, error) {
	size, err := unix.Getxattr(path, name, nil)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	n, err := unix.Getxattr(path, name, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func isXattrUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENODATA)
}

// splitXattrNames splits Listxattr's NUL-separated name list into
// individual attribute names.
func splitXattrNames(buf []byte) []string {
	var names []string
	start := 0
	for i, b := range buf {
		if b == 0 {
			if i > start {
				names = append(names, string(buf[start:i]))
			}
			start = i + 1
		}
	}
	return names
}
