// Package template defines the x-hoserva template format (doc 04 §7, Q64):
// a Compose file with an x-hoserva extension block. It owns the block's
// schema, the parser and validator that enforce it, the path conventions,
// and the JSON Schema published for third-party catalog authors. The
// catalog repository's CI runs this package through `hoserva template lint`.
// It also resolves a template's inputs and installs it as a stack, with the
// privilege summary read from the Compose content (Installer).
package template

import (
	"github.com/invopop/jsonschema"
)

// Input kinds and roles (doc 04 §7).
const (
	KindPath     = "path"
	KindPort     = "port"
	KindString   = "string"
	KindSecret   = "secret"
	KindTimezone = "timezone"
	KindDevice   = "device"

	RoleAppdata   = "appdata"
	RoleShare     = "share"
	RoleMedia     = "media"
	RoleDownloads = "downloads"
	RoleGPU       = "gpu"
)

// Block is the x-hoserva extension block of schema version 1. It is the one
// definition the validator and the published JSON Schema are both built from.
type Block struct {
	Schema     int              `json:"schema" yaml:"schema" jsonschema:"enum=1,description=Schema version of this block. A newer Hoserva keeps reading older versions."`
	ID         string           `json:"id" yaml:"id" jsonschema:"pattern=^[a-z0-9]+(-[a-z0-9]+)*$,description=Template id. Equals the name of the directory holding compose.yaml."`
	Revision   int              `json:"revision" yaml:"revision" jsonschema:"minimum=1,description=Increases with every change to the template."`
	Title      string           `json:"title" yaml:"title" jsonschema:"minLength=1,description=Name shown in the catalog."`
	Categories []string         `json:"categories" yaml:"categories" jsonschema:"minItems=1,uniqueItems=true,pattern=^[a-z0-9]+(-[a-z0-9]+)*$,description=Catalog categories."`
	Icon       string           `json:"icon" yaml:"icon" jsonschema:"pattern=^[A-Za-z0-9][A-Za-z0-9._-]*$,description=File name of the icon next to compose.yaml."`
	Docs       string           `json:"docs" yaml:"docs" jsonschema:"pattern=^https?://,description=Upstream documentation the template was written from."`
	WebUI      string           `json:"webui,omitempty" yaml:"webui,omitempty" jsonschema:"pattern=^https?://,description=Address of the app's web interface. {host} stands for the server's address and ${INPUT} for an input."`
	Inputs     map[string]Input `json:"inputs,omitempty" yaml:"inputs,omitempty" jsonschema:"description=The only values the install form asks for. Each key is a variable name. Every input except a device input must be used: referenced as ${NAME} in the Compose file or the webui address; or passed to a service through env_file: .env. A device input needs no reference."`
}

// Input is one value the install form asks for.
type Input struct {
	Kind        string `json:"kind" yaml:"kind" jsonschema:"enum=path,enum=port,enum=string,enum=secret,enum=timezone,enum=device,description=What the value is."`
	Role        string `json:"role,omitempty" yaml:"role,omitempty" jsonschema:"enum=appdata,enum=share,enum=media,enum=downloads,enum=gpu,description=Required for path (appdata/share/media/downloads) and device (gpu) inputs; not allowed for other kinds."`
	Default     any    `json:"default,omitempty" yaml:"default,omitempty" jsonschema:"oneof_type=string;integer,description=Preset value. A secret has none; it is generated at install time."`
	Label       string `json:"label,omitempty" yaml:"label,omitempty" jsonschema:"minLength=1,description=Plain-language name shown in the install form."`
	Description string `json:"description,omitempty" yaml:"description,omitempty" jsonschema:"minLength=1,description=Help text shown in the install form."`
}

// JSONSchemaExtend adds the rules that depend on more than one field.
func (Input) JSONSchemaExtend(s *jsonschema.Schema) {
	whenKind := func(kinds ...any) map[string]any {
		kind := map[string]any{"const": kinds[0]}
		if len(kinds) > 1 {
			kind = map[string]any{"enum": kinds}
		}
		return map[string]any{
			"properties": map[string]any{"kind": kind},
			"required":   []any{"kind"},
		}
	}
	if s.Extras == nil {
		s.Extras = map[string]any{}
	}
	s.Extras["allOf"] = []any{
		map[string]any{
			"if": whenKind(KindPath),
			"then": map[string]any{
				"required": []any{"role"},
				"properties": map[string]any{
					"role":    map[string]any{"enum": []any{RoleAppdata, RoleShare, RoleMedia, RoleDownloads}},
					"default": map[string]any{"type": "string", "pattern": "^/"},
				},
			},
		},
		map[string]any{
			"if": whenKind(KindDevice),
			"then": map[string]any{
				"required":   []any{"role"},
				"properties": map[string]any{"role": map[string]any{"const": RoleGPU}},
			},
		},
		map[string]any{
			"if":   whenKind(KindPort, KindString, KindSecret, KindTimezone),
			"then": map[string]any{"not": map[string]any{"required": []any{"role"}}},
		},
		map[string]any{
			"if": whenKind(KindPort),
			"then": map[string]any{
				"properties": map[string]any{"default": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535}},
			},
		},
		map[string]any{
			"if":   whenKind(KindSecret),
			"then": map[string]any{"not": map[string]any{"required": []any{"default"}}},
		},
	}
}

// JSONSchemaExtend restricts input names to the variable names Compose
// interpolation accepts.
func (Block) JSONSchemaExtend(s *jsonschema.Schema) {
	if p, ok := s.Properties.Get("inputs"); ok {
		if p.Extras == nil {
			p.Extras = map[string]any{}
		}
		p.Extras["propertyNames"] = map[string]any{"pattern": "^[A-Z][A-Z0-9_]*$"}
	}
}
