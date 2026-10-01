package template

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/mdg-labs/hoserva/internal/container"
)

// StackCreator stores a stack and generates its files, and reports the host
// ports the existing stacks publish, started or not
// (container.StackService).
type StackCreator interface {
	Create(ctx context.Context, n container.NewStack) (container.Stack, error)
	PublishedPorts(ctx context.Context) (map[int]bool, error)
}

// Installer resolves a template's inputs and installs it as a stack (doc 04
// §7). Catalog, Stacks and Ports are required; the rest are optional.
type Installer struct {
	Catalog Catalog
	Stacks  StackCreator
	Ports   PortSource
	// Shares lists the names of the existing shares, which paths default to
	// and are offered from. Nil offers none.
	Shares func(ctx context.Context) ([]string, error)
	// GPU offers the host's render devices to a device input. Nil offers
	// none.
	GPU GPUHost
	// Timezone is the host's time zone, the default of a timezone input
	// that declares none. Nil or empty means UTC.
	Timezone func() string
	// Random is the source of generated secrets; nil means crypto/rand.
	Random io.Reader

	mu sync.Mutex
}

// PlanRequest names a template and the values the user chose. An input that
// has no value here takes its default.
type PlanRequest struct {
	ID string
	// Name is the stack's name; empty means the template's id.
	Name   string
	Values map[string]string
}

// ResolvedInput is one input with the value it resolves to.
type ResolvedInput struct {
	Name        string
	Kind        string
	Role        string
	Label       string
	Description string
	// Value is empty for a secret, whose value only ever goes to the .env.
	Value string
	// Requested is the port that was asked for when Value is the next free
	// port instead.
	Requested string
	// Generated marks a secret that is generated at install.
	Generated bool
	// Suggestions are the existing shares' paths for a path input and the
	// host's render devices for a device input.
	Suggestions []string
}

// Plan is what installing a template would do.
type Plan struct {
	Source     string
	ID         string
	Revision   int
	Title      string
	Name       string
	Inputs     []ResolvedInput
	Privileges []Privilege
	// Compose is the docker-compose.yml that is written: the template with
	// its x-hoserva block, plus the GPU mapping when one was chosen.
	Compose string

	env string
}

// Preview resolves the inputs and computes the privilege summary. It creates
// no stack and writes no file of the template. Reading the other stacks' ports
// writes back any of their missing generated files from their rows, and never
// changes an existing file. Secrets are not generated.
func (in *Installer) Preview(ctx context.Context, req PlanRequest) (*Plan, error) {
	return in.plan(ctx, req, false)
}

// Install resolves the inputs, generates the secrets and creates the stack:
// its row, docker-compose.yml, .env and meta.json recording the source, id
// and revision. Nothing is started. A port is taken when a container
// publishes or is configured to publish it, an existing stack's Compose file
// publishes it (started or not), or the host listens on it, so an install is
// never given a port an earlier install's stack already holds; installs run
// one at a time so the stack is stored before the next one reads the ports.
// A port something else takes after the install is not checked. Reading the
// other stacks' ports writes back any of their missing generated files from
// their rows, and never changes an existing file.
func (in *Installer) Install(ctx context.Context, req PlanRequest) (*Plan, container.Stack, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	p, err := in.plan(ctx, req, true)
	if err != nil {
		return nil, container.Stack{}, err
	}
	st, err := in.Stacks.Create(ctx, container.NewStack{
		Name:             p.Name,
		Compose:          p.Compose,
		Env:              p.env,
		TemplateSource:   p.Source,
		TemplateID:       p.ID,
		TemplateRevision: strconv.Itoa(p.Revision),
	})
	if err != nil {
		return nil, container.Stack{}, err
	}
	return p, st, nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

func (in *Installer) load(ctx context.Context, id string) (Entry, *Template, error) {
	return loadTemplate(ctx, in.Catalog, id)
}

// loadTemplate reads a catalog entry and refuses it unless it passes the
// schema and the template rules and names the id it was found under.
func loadTemplate(ctx context.Context, c Catalog, id string) (Entry, *Template, error) {
	entry, err := c.Entry(ctx, id)
	if err != nil {
		return Entry{}, nil, err
	}
	t, issues := Parse(entry.Data)
	if t == nil {
		return Entry{}, nil, invalidTemplate(id, issues)
	}
	if issues := t.Check(); len(issues) > 0 {
		return Entry{}, nil, invalidTemplate(id, issues)
	}
	if t.Block.ID != id {
		return Entry{}, nil, fmt.Errorf("%w: %s: x-hoserva.id is %q", ErrInvalidTemplate, id, t.Block.ID)
	}
	return entry, t, nil
}

func invalidTemplate(id string, issues []Issue) error {
	msgs := make([]string, len(issues))
	for i, is := range issues {
		msgs[i] = is.String()
	}
	return fmt.Errorf("%w: %s: %s", ErrInvalidTemplate, id, strings.Join(msgs, "; "))
}

// usedPorts is every port that is taken: what Ports reports and what the
// existing stacks publish. It fails when either cannot be read.
func (in *Installer) usedPorts(ctx context.Context) (map[int]bool, error) {
	used, err := in.Ports.UsedPorts(ctx)
	if err != nil {
		return nil, err
	}
	stacked, err := in.Stacks.PublishedPorts(ctx)
	if err != nil {
		return nil, err
	}
	all := make(map[int]bool, len(used)+len(stacked))
	for p := range used {
		all[p] = true
	}
	for p := range stacked {
		all[p] = true
	}
	return all, nil
}

func (in *Installer) plan(ctx context.Context, req PlanRequest, generate bool) (*Plan, error) {
	entry, t, err := in.load(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	name := req.Name
	if name == "" {
		name = t.Block.ID
	}
	if !container.ValidStackName(name) {
		return nil, fmt.Errorf("%w: %q", container.ErrInvalidStackName, name)
	}
	for _, k := range sortedKeys(req.Values) {
		if _, ok := t.Block.Inputs[k]; !ok {
			return nil, invalid("%s is not an input of template %s", k, t.Block.ID)
		}
	}

	var (
		inputs  = sortedKeys(t.Block.Inputs)
		used    map[int]bool
		taken   = map[int]bool{}
		shares  []string
		devices []string
		haveGPU bool
		values  = make(map[string]string, len(inputs))
		secrets = map[string]string{}
		pending = map[string]bool{}
		out     = make([]ResolvedInput, 0, len(inputs))
		compose = entry.Data
	)
	for _, n := range inputs {
		switch t.Block.Inputs[n].Kind {
		case KindPort:
			if used == nil {
				if used, err = in.usedPorts(ctx); err != nil {
					return nil, fmt.Errorf("checking the ports for conflicts: %w", err)
				}
			}
		case KindPath:
			if shares == nil && t.Block.Inputs[n].Role != RoleAppdata && in.Shares != nil {
				if shares, err = in.Shares(ctx); err != nil {
					return nil, fmt.Errorf("listing the shares: %w", err)
				}
				sort.Strings(shares)
			}
		case KindDevice:
			if !haveGPU && in.GPU != nil {
				if devices, err = in.GPU.RenderDevices(ctx); err != nil {
					return nil, err
				}
				haveGPU = true
			}
		}
	}

	for _, n := range inputs {
		spec := t.Block.Inputs[n]
		ri := ResolvedInput{Name: n, Kind: spec.Kind, Role: spec.Role, Label: spec.Label, Description: spec.Description}
		given := req.Values[n]
		def := defaultString(spec.Default)
		switch spec.Kind {
		case KindPath:
			v := firstNonEmpty(given, def)
			if v == "" && (spec.Role == RoleMedia || spec.Role == RoleDownloads) && contains(shares, spec.Role) {
				v = poolRoot + "/" + spec.Role
			}
			if v == "" {
				return nil, invalid("%s needs a path", n)
			}
			if !strings.HasPrefix(v, "/") {
				return nil, invalid("%s must be an absolute path, got %q", n, v)
			}
			ri.Value = path.Clean(v)
			if spec.Role != RoleAppdata {
				for _, s := range shares {
					ri.Suggestions = append(ri.Suggestions, poolRoot+"/"+s)
				}
			}
		case KindPort:
			v := firstNonEmpty(given, def)
			if v == "" {
				return nil, invalid("%s needs a port", n)
			}
			p, perr := strconv.Atoi(v)
			if perr != nil || p < 1 || p > 65535 {
				return nil, invalid("%s must be a port from 1 to 65535, got %q", n, v)
			}
			if used[p] || taken[p] {
				next, ok := nextFreePort(p, used, taken)
				if !ok {
					return nil, fmt.Errorf("%w: %s asked for %d", ErrNoFreePort, n, p)
				}
				ri.Requested = strconv.Itoa(p)
				p = next
			}
			taken[p] = true
			ri.Value = strconv.Itoa(p)
		case KindSecret:
			ri.Generated = given == ""
			if !ri.Generated {
				secrets[n] = given
			} else if generate {
				if secrets[n], err = in.newSecret(); err != nil {
					return nil, err
				}
			} else {
				pending[n] = true
			}
		case KindTimezone:
			v := firstNonEmpty(given, def)
			if v == "" && in.Timezone != nil {
				v = in.Timezone()
			}
			if v == "" {
				v = "UTC"
			}
			if !timezonePattern.MatchString(v) || strings.Contains(v, "..") {
				return nil, invalid("%s must be a time zone name such as Europe/Vienna, got %q", n, v)
			}
			ri.Value = v
		case KindDevice:
			v := firstNonEmpty(given, def)
			ri.Suggestions = devices
			if v != "" {
				if !contains(devices, v) {
					return nil, invalid("%s: %q is not one of the host's GPU render devices %v", n, v, devices)
				}
				gid, gerr := in.GPU.RenderGID(ctx)
				if gerr != nil {
					return nil, gerr
				}
				if compose, err = addGPU(compose, v, gid); err != nil {
					return nil, err
				}
			}
			ri.Value = v
		default:
			v := firstNonEmpty(given, def)
			if v == "" && !spec.Optional {
				return nil, invalid("%s needs a value", n)
			}
			ri.Value = v
		}
		if ri.Kind != KindSecret {
			values[n] = ri.Value
		} else {
			values[n] = secrets[n]
		}
		if err := checkEnvValue(n, values[n]); err != nil {
			return nil, err
		}
		out = append(out, ri)
	}

	var env strings.Builder
	for _, n := range inputs {
		env.WriteString(n + "=" + quoteEnv(values[n]) + "\n")
	}
	// A secret a preview has not generated yet still has a value once it is
	// installed, so the summary is computed with one of the generated shape.
	summed := make(map[string]string, len(values))
	for k, v := range values {
		summed[k] = v
		if pending[k] {
			summed[k] = secretPlaceholder
		}
	}
	if err := t.checkResolved(summed); err != nil {
		return nil, err
	}
	return &Plan{
		Source:     entry.Source,
		ID:         t.Block.ID,
		Revision:   t.Block.Revision,
		Title:      t.Block.Title,
		Name:       name,
		Inputs:     out,
		Privileges: t.Privileges(summed),
		Compose:    string(compose),
		env:        env.String(),
	}, nil
}

var timezonePattern = regexp.MustCompile(`^[A-Za-z0-9_+/-]+$`)

func defaultString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	}
	return ""
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

const secretBytes = 24

// secretPlaceholder has the length and alphabet of a generated secret.
var secretPlaceholder = strings.Repeat("0", 2*secretBytes)

func (in *Installer) newSecret() (string, error) {
	r := in.Random
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, secretBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("generating a secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// checkEnvValue refuses a value the .env file cannot hold as written: a line
// break would start another variable, and a single quote ends the quoting
// quoteEnv relies on.
func checkEnvValue(name, v string) error {
	if strings.ContainsAny(v, "\n\r\x00'") {
		return invalid("%s must not contain a line break or a single quote", name)
	}
	return nil
}

var plainEnvValue = regexp.MustCompile(`^[A-Za-z0-9_./:@%+,=-]*$`)

// quoteEnv writes a value so Compose reads it back exactly: single quotes
// switch off interpolation and every escape.
func quoteEnv(v string) string {
	if plainEnvValue.MatchString(v) {
		return v
	}
	return "'" + v + "'"
}

// addGPU maps a render device into every service and adds the host's render
// group (Q82). A template's inputs name no service, so every service gets
// the device.
func addGPU(compose []byte, device, gid string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(compose, &doc); err != nil {
		return nil, fmt.Errorf("%w: not valid YAML: %v", ErrInvalidTemplate, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, fmt.Errorf("%w: not a Compose document", ErrInvalidTemplate)
	}
	services := mappingValue(doc.Content[0], "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: no services section", ErrInvalidTemplate)
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		svc := services.Content[i+1]
		if svc.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%w: service %s is not a mapping, so it cannot be given the GPU", ErrInvalidTemplate, services.Content[i].Value)
		}
		if err := appendToList(svc, "devices", device+":"+device); err != nil {
			return nil, fmt.Errorf("%w: service %s: %v", ErrInvalidTemplate, services.Content[i].Value, err)
		}
		if err := appendToList(svc, "group_add", gid); err != nil {
			return nil, fmt.Errorf("%w: service %s: %v", ErrInvalidTemplate, services.Content[i].Value, err)
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("writing the Compose file: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("writing the Compose file: %w", err)
	}
	return buf.Bytes(), nil
}

// appendToList adds a string to the list under key in a service mapping,
// creating the list, unless the list already holds it.
func appendToList(svc *yaml.Node, key, value string) error {
	item := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle}
	list := mappingValue(svc, key)
	if list == nil {
		svc.Content = append(svc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{item}})
		return nil
	}
	if list.Kind != yaml.SequenceNode {
		return fmt.Errorf("%s is not a list", key)
	}
	for _, e := range list.Content {
		if e.Value == value {
			return nil
		}
	}
	list.Content = append(list.Content, item)
	return nil
}
