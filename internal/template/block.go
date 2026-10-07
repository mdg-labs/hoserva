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

	FormatHex        = "hex"
	FormatLaravelKey = "laravel-key"

	RoleAppdata   = "appdata"
	RoleShare     = "share"
	RoleMedia     = "media"
	RoleDownloads = "downloads"
	RoleGPU       = "gpu"
)

// Block is the x-hoserva extension block of schema version 1. It is the one
// definition the validator and the published JSON Schema are both built from.
type Block struct {
	Schema      int              `json:"schema" yaml:"schema" jsonschema:"enum=1,description=Schema version of this block. A newer Hoserva keeps reading older versions."`
	ID          string           `json:"id" yaml:"id" jsonschema:"pattern=^[a-z0-9]+(-[a-z0-9]+)*$,description=Template id. Equals the name of the directory holding compose.yaml."`
	Revision    int              `json:"revision" yaml:"revision" jsonschema:"minimum=1,description=Increases with every change to the template."`
	Title       string           `json:"title" yaml:"title" jsonschema:"minLength=1,description=Name shown in the catalog."`
	Categories  []string         `json:"categories" yaml:"categories" jsonschema:"minItems=1,uniqueItems=true,pattern=^[a-z0-9]+(-[a-z0-9]+)*$,description=Catalog categories."`
	Icon        string           `json:"icon,omitempty" yaml:"icon,omitempty" jsonschema:"pattern=^[A-Za-z0-9][A-Za-z0-9._-]*$,description=File name of the icon next to compose.yaml. Omitted only when no usable icon exists; Hoserva then shows a built-in placeholder."`
	Docs        string           `json:"docs" yaml:"docs" jsonschema:"pattern=^https?://,description=Upstream documentation the template was written from."`
	Maintainer  string           `json:"maintainer,omitempty" yaml:"maintainer,omitempty" jsonschema:"minLength=1,maxLength=100,description=Who maintains the template or the app. Shown in the catalog and used as a filter."`
	Description string           `json:"description,omitempty" yaml:"description,omitempty" jsonschema:"minLength=1,maxLength=2000,description=Longer plain-text description shown on the template's page. Line breaks are kept; nothing is interpreted as markup."`
	Screenshots []string         `json:"screenshots,omitempty" yaml:"screenshots,omitempty" jsonschema:"minItems=1,maxItems=8,uniqueItems=true,maxLength=200,pattern=^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*[.](png|webp|jpg|jpeg)$,description=Image files of the template's directory as paths relative to compose.yaml (PNG or WebP or JPEG files). No segment may start with a dot."`
	Links       Links            `json:"links,omitempty" yaml:"links,omitempty" jsonschema:"description=Where to find the app's project page and ways to support it. Every link is an https address."`
	WebUI       string           `json:"webui,omitempty" yaml:"webui,omitempty" jsonschema:"pattern=^https?://,description=Address of the app's web interface. {host} stands for the server's address and ${INPUT} for an input."`
	Inputs      map[string]Input `json:"inputs,omitempty" yaml:"inputs,omitempty" jsonschema:"description=The only values the install form asks for. Each key is a variable name. Every input except a device input must be used: referenced as ${NAME} in the Compose file or the webui address; or passed to a service through env_file: .env. A device input needs no reference."`
}

// Links are the optional addresses a template's page offers.
type Links struct {
	Project string `json:"project,omitempty" yaml:"project,omitempty" jsonschema:"pattern=^https://,maxLength=2048,description=The app's project page."`
	Support string `json:"support,omitempty" yaml:"support,omitempty" jsonschema:"pattern=^https://,maxLength=2048,description=Where to get help with the app."`
	Donate  string `json:"donate,omitempty" yaml:"donate,omitempty" jsonschema:"pattern=^https://,maxLength=2048,description=Where to support the app's development."`
}

// Input is one value the install form asks for.
type Input struct {
	Kind        string `json:"kind" yaml:"kind" jsonschema:"enum=path,enum=port,enum=string,enum=secret,enum=timezone,enum=device,description=What the value is."`
	Role        string `json:"role,omitempty" yaml:"role,omitempty" jsonschema:"enum=appdata,enum=share,enum=media,enum=downloads,enum=gpu,description=Required for path (appdata/share/media/downloads) and device (gpu) inputs; not allowed for other kinds."`
	Default     any    `json:"default,omitempty" yaml:"default,omitempty" jsonschema:"oneof_type=string;integer,description=Preset value. A secret has none; it is generated at install time."`
	Label       string `json:"label,omitempty" yaml:"label,omitempty" jsonschema:"minLength=1,description=Plain-language name shown in the install form."`
	Description string `json:"description,omitempty" yaml:"description,omitempty" jsonschema:"minLength=1,description=Help text shown in the install form."`
	Format      string `json:"format,omitempty" yaml:"format,omitempty" jsonschema:"enum=hex,enum=laravel-key,description=Only for secret inputs: the shape of a generated value. hex (the default) is 48 hexadecimal characters; laravel-key is base64: followed by 32 random bytes in base64 - the APP_KEY of a Laravel application. A value typed for a laravel-key secret must have the same shape; any value is accepted for hex."`
	Optional    bool   `json:"optional,omitempty" yaml:"optional,omitempty" jsonschema:"description=Only for string inputs: the value may be left empty and is then written to .env as an empty value. An optional input has no default."`
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
		map[string]any{
			"if":   whenKind(KindPath, KindPort, KindSecret, KindTimezone, KindDevice),
			"then": map[string]any{"not": map[string]any{"required": []any{"optional"}}},
		},
		map[string]any{
			"if":   whenKind(KindPath, KindPort, KindString, KindTimezone, KindDevice),
			"then": map[string]any{"not": map[string]any{"required": []any{"format"}}},
		},
		map[string]any{
			"if": map[string]any{
				"properties": map[string]any{"optional": map[string]any{"const": true}},
				"required":   []any{"optional"},
			},
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
