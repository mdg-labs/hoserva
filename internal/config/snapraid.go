// Package config renders Hoserva's generated config files from state (D4):
// state in, text out, nothing hand-edited.
package config

import (
	"fmt"
	"strings"
)

// DataDisk is one data disk entry in a snapraid.conf.
type DataDisk struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// SnapraidState is the slice of pool state RenderSnapraidConf needs to
// render a snapraid.conf (doc 02 §2). It stands in for the SQLite-backed
// state the real config generator will read from the store; this issue
// builds the golden-file test infrastructure, not that generator.
type SnapraidState struct {
	Parity       string     `json:"parity"`
	ContentPaths []string   `json:"content_paths"`
	DataDisks    []DataDisk `json:"data_disks"`
	Excludes     []string   `json:"excludes"`
}

// RenderSnapraidConf renders a snapraid.conf from state. Directive order
// follows snapraid's own convention: parity, content, data, exclude.
func RenderSnapraidConf(s SnapraidState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "parity %s\n", s.Parity)
	for _, c := range s.ContentPaths {
		fmt.Fprintf(&b, "content %s\n", c)
	}
	for _, d := range s.DataDisks {
		fmt.Fprintf(&b, "data %s %s\n", d.Name, d.Path)
	}
	for _, e := range s.Excludes {
		fmt.Fprintf(&b, "exclude %s\n", e)
	}
	return b.String()
}
