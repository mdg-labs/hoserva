package template

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/invopop/jsonschema"
	santhosh "github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// Every schema version this Hoserva reads. A newer Hoserva keeps the older
// entries, so the `schema:` number is the compatibility boundary (Q39).
var versions = map[int]any{
	1: Block{},
}

const schemaIDBase = "https://raw.githubusercontent.com/mdg-labs/hoserva/main/internal/template/schema/"

// SupportedVersions lists the schema versions this Hoserva reads, ascending.
func SupportedVersions() []int {
	out := make([]int, 0, len(versions))
	for v := range versions {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// JSONSchema returns the published JSON Schema of the x-hoserva block for
// one schema version, generated from the Go definition the validator uses.
// The same bytes are committed as schema/v<version>.json.
func JSONSchema(version int) ([]byte, error) {
	def, ok := versions[version]
	if !ok {
		return nil, fmt.Errorf("x-hoserva schema version %d is not supported", version)
	}
	r := &jsonschema.Reflector{DoNotReference: true, ExpandedStruct: true}
	r.SetBaseSchemaID(schemaIDBase + "v" + strconv.Itoa(version) + ".json")
	s := r.Reflect(def)
	s.ID = jsonschema.ID(schemaIDBase + "v" + strconv.Itoa(version) + ".json")
	s.Title = "Hoserva x-hoserva template block, schema version " + strconv.Itoa(version)
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the x-hoserva schema: %w", err)
	}
	return append(out, '\n'), nil
}

// compileSchema compiles the JSON Schema of one version.
func compileSchema(version int) (*santhosh.Schema, error) {
	raw, err := JSONSchema(version)
	if err != nil {
		return nil, err
	}
	doc, err := santhosh.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("reading the x-hoserva schema: %w", err)
	}
	url := schemaIDBase + "v" + strconv.Itoa(version) + ".json"
	c := santhosh.NewCompiler()
	if err := c.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("loading the x-hoserva schema: %w", err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("compiling the x-hoserva schema: %w", err)
	}
	return sch, nil
}

var printer = message.NewPrinter(language.English)

// schemaViolations flattens a validation error to its leaf messages, each
// with the instance location it applies to.
func schemaViolations(err *santhosh.ValidationError) []Issue {
	if len(err.Causes) == 0 {
		return []Issue{{Path: append([]string(nil), err.InstanceLocation...), Message: err.ErrorKind.LocalizedString(printer)}}
	}
	var out []Issue
	for _, c := range err.Causes {
		out = append(out, schemaViolations(c)...)
	}
	return out
}
