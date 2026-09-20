//go:build !linux

package share

func lgetxattr(path, attr string) ([]byte, error) {
	return nil, nil
}
