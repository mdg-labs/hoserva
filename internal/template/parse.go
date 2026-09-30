package template

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	santhosh "github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// BlockKey is the Compose extension field that holds the template block.
const BlockKey = "x-hoserva"

// Issue is one problem found in a template, located by its path in the
// Compose document (for example x-hoserva, inputs, TZ, kind).
type Issue struct {
	Path    []string
	Line    int
	Message string
}

func (i Issue) String() string {
	var sb strings.Builder
	if i.Line > 0 {
		sb.WriteString("line ")
		sb.WriteString(strconv.Itoa(i.Line))
		sb.WriteString(": ")
	}
	if len(i.Path) > 0 {
		sb.WriteString(strings.Join(i.Path, "."))
		sb.WriteString(": ")
	}
	sb.WriteString(i.Message)
	return sb.String()
}

// Template is a parsed compose.yaml whose x-hoserva block passed schema
// validation.
type Template struct {
	Block   Block
	Compose map[string]any
	root    *yaml.Node
}

// Parse reads a compose.yaml, validates its x-hoserva block against the
// schema version it declares, and returns either the template or every
// schema problem found. Rules beyond the schema run in Check.
func Parse(data []byte) (*Template, []Issue) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, []Issue{{Message: fmt.Sprintf("not valid YAML: %v", err)}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, []Issue{{Message: "compose.yaml must be a mapping with a services section and an x-hoserva block"}}
	}
	root := doc.Content[0]

	var compose map[string]any
	if err := root.Decode(&compose); err != nil {
		return nil, []Issue{{Message: fmt.Sprintf("not valid Compose YAML: %v", err)}}
	}

	issues := func(is []Issue) []Issue {
		for i := range is {
			is[i].Line = lineOf(root, is[i].Path)
		}
		return is
	}

	rawBlock, ok := compose[BlockKey]
	if !ok {
		return nil, []Issue{{Message: "no " + BlockKey + " block: this is not a Hoserva template"}}
	}
	block, ok := rawBlock.(map[string]any)
	if !ok {
		return nil, issues([]Issue{{Path: []string{BlockKey}, Message: "must be a mapping"}})
	}
	version, bad := blockVersion(block)
	if bad != nil {
		return nil, issues([]Issue{*bad})
	}

	sch, err := schemaFor(version)
	if err != nil {
		return nil, issues([]Issue{{Path: []string{BlockKey, "schema"}, Message: err.Error()}})
	}
	encoded, err := json.Marshal(block)
	if err != nil {
		return nil, issues([]Issue{{Path: []string{BlockKey}, Message: fmt.Sprintf("holds a value that is not plain data: %v", err)}})
	}
	instance, err := santhosh.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, issues([]Issue{{Path: []string{BlockKey}, Message: err.Error()}})
	}
	if err := sch.Validate(instance); err != nil {
		ve, ok := err.(*santhosh.ValidationError)
		if !ok {
			return nil, issues([]Issue{{Path: []string{BlockKey}, Message: err.Error()}})
		}
		found := schemaViolations(ve)
		for i := range found {
			found[i].Path = append([]string{BlockKey}, found[i].Path...)
		}
		return nil, issues(found)
	}

	t := &Template{Compose: compose, root: root}
	if err := blockNode(root).Decode(&t.Block); err != nil {
		return nil, issues([]Issue{{Path: []string{BlockKey}, Message: err.Error()}})
	}
	return t, nil
}

func blockVersion(block map[string]any) (int, *Issue) {
	path := []string{BlockKey, "schema"}
	raw, ok := block["schema"]
	if !ok {
		return 0, &Issue{Path: path, Message: "is required: the schema version of this block, currently " + supportedList()}
	}
	v, ok := raw.(int)
	if !ok {
		return 0, &Issue{Path: path, Message: fmt.Sprintf("must be an integer schema version, got %v", raw)}
	}
	if _, ok := versions[v]; !ok {
		return 0, &Issue{Path: path, Message: fmt.Sprintf("schema version %d is not supported; this Hoserva reads %s", v, supportedList())}
	}
	return v, nil
}

func supportedList() string {
	vs := SupportedVersions()
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ", ")
}

var (
	schemaMu    sync.Mutex
	schemaCache = map[int]*santhosh.Schema{}
)

func schemaFor(version int) (*santhosh.Schema, error) {
	schemaMu.Lock()
	defer schemaMu.Unlock()
	if s, ok := schemaCache[version]; ok {
		return s, nil
	}
	s, err := compileSchema(version)
	if err != nil {
		return nil, err
	}
	schemaCache[version] = s
	return s, nil
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func blockNode(root *yaml.Node) *yaml.Node {
	return mappingValue(root, BlockKey)
}

// lineOf returns the line of the node at path, or of the deepest node that
// exists on it, so an issue about a missing field points at its parent.
func lineOf(root *yaml.Node, path []string) int {
	n := root
	for _, tok := range path {
		var next *yaml.Node
		switch n.Kind {
		case yaml.MappingNode:
			next = mappingValue(n, tok)
		case yaml.SequenceNode:
			if i, err := strconv.Atoi(tok); err == nil && i >= 0 && i < len(n.Content) {
				next = n.Content[i]
			}
		case yaml.AliasNode:
			next = n.Alias
		}
		if next == nil {
			break
		}
		n = next
	}
	return n.Line
}
