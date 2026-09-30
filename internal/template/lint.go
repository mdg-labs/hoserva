package template

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ComposeFile is the file every template directory holds.
const ComposeFile = "compose.yaml"

// Finding is one problem in a catalog checkout. File is relative to the
// checked directory.
type Finding struct {
	File string
	Issue
}

func (f Finding) String() string {
	return f.File + ": " + f.Issue.String()
}

// Lint checks every <id>/compose.yaml under dir: the x-hoserva schema, the
// rules beyond it, and the directory conventions (the id is the directory's
// name, the icon exists). It returns the findings in a stable order, and an
// error only when dir itself cannot be read.
func Lint(dir string) ([]Finding, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading the catalog directory: %w", err)
	}
	var out []Finding
	found := 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		found++
		out = append(out, lintTemplate(dir, e.Name())...)
	}
	if found == 0 {
		out = append(out, Finding{File: ".", Issue: Issue{Message: "no template directories found; expected <id>/" + ComposeFile}})
	}
	return out, nil
}

func lintTemplate(dir, id string) []Finding {
	file := filepath.Join(id, ComposeFile)
	at := func(found ...Issue) []Finding {
		out := make([]Finding, len(found))
		for i, is := range found {
			out[i] = Finding{File: file, Issue: is}
		}
		return out
	}

	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return at(Issue{Message: ComposeFile + " is missing"})
		}
		return at(Issue{Message: fmt.Sprintf("cannot read %s: %v", ComposeFile, err)})
	}
	t, issues := Parse(data)
	if t == nil {
		return at(issues...)
	}
	issues = t.Check()
	if t.Block.ID != id {
		issues = append(issues, Issue{Path: []string{BlockKey, "id"}, Line: lineOf(t.root, []string{BlockKey, "id"}),
			Message: fmt.Sprintf("is %q but the directory is named %q; they must match", t.Block.ID, id)})
	}
	icon := filepath.Join(dir, id, t.Block.Icon)
	if info, err := os.Lstat(icon); err != nil || !info.Mode().IsRegular() {
		issues = append(issues, Issue{Path: []string{BlockKey, "icon"}, Line: lineOf(t.root, []string{BlockKey, "icon"}),
			Message: fmt.Sprintf("names %q, which is not a file next to %s", t.Block.Icon, ComposeFile)})
	}
	return at(issues...)
}
