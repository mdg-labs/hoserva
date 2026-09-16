package store

import (
	"os"
	"testing"
)

// schemaSQLForTest reads the real, committed schema.sql — go test's
// working directory is the package directory, so this is a plain relative
// path, not an assumption about the repository root.
func schemaSQLForTest(t *testing.T) (string, error) {
	t.Helper()
	data, err := os.ReadFile("schema/schema.sql")
	if err != nil {
		return "", err
	}
	return string(data), nil
}
