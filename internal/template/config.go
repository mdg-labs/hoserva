package template

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mdg-labs/hoserva/internal/container"
)

// ConfigInput is one input of an installed stack with the value it has now.
type ConfigInput struct {
	Name        string
	Kind        string
	Role        string
	Label       string
	Description string
	// Value is empty for a secret, whose value never leaves the .env.
	Value string
	// Set reports, for a secret, whether the stack's .env gives it a value.
	Set bool
	// ReadOnly is true for a device input: its mapping lives in the stack's
	// Compose file, which the form never writes.
	ReadOnly bool
	// Suggestions are the existing shares' paths for a path input that is
	// not appdata.
	Suggestions []string
}

// StackConfig is an installed stack's template inputs.
type StackConfig struct {
	Stack  container.Stack
	Inputs []ConfigInput
}

// ConfigUpdate names the inputs to change. An input with no entry keeps its
// value. A secret with an empty or no entry keeps its sealed value; a
// secret in Generate gets a newly generated one.
type ConfigUpdate struct {
	Values   map[string]string
	Generate []string
}

// Config reads the inputs of an installed stack from the x-hoserva block of
// its stored Compose text, which is the file as it runs, edited by hand or
// not, and their values from the stack's .env. It writes nothing and needs
// no catalog.
func (in *Installer) Config(ctx context.Context, name string) (*StackConfig, error) {
	st, t, env, err := in.loadInstalled(ctx, name)
	if err != nil {
		return nil, err
	}
	return in.configOf(ctx, st, t, parseEnv(env))
}

// UpdateConfig validates the changed values with the rules an install uses
// and, only if all of them hold, replaces the stack's .env: the lines of the
// inputs that changed are rewritten and every other line is kept. The
// stack's Compose file is never touched, so a manual edit of it survives.
// Nothing is restarted. A port input changed to a port that is in use is
// refused with ErrPortTaken, a device input cannot be changed, and nothing
// is stored on any refusal.
func (in *Installer) UpdateConfig(ctx context.Context, name string, req ConfigUpdate) (*StackConfig, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	st, t, env, err := in.loadInstalled(ctx, name)
	if err != nil {
		return nil, err
	}
	generate := make(map[string]bool, len(req.Generate))
	for _, n := range sortedKeys(req.Values) {
		if _, ok := t.Block.Inputs[n]; !ok {
			return nil, invalidInput(n, "%s is not an input of stack %s", n, name)
		}
	}
	for _, n := range req.Generate {
		spec, ok := t.Block.Inputs[n]
		switch {
		case !ok:
			return nil, invalidInput(n, "%s is not an input of stack %s", n, name)
		case spec.Kind != KindSecret:
			return nil, invalidInput(n, "%s is not a secret, so it cannot be generated", n)
		case req.Values[n] != "":
			return nil, invalidInput(n, "%s is given a value and also asked to be generated", n)
		}
		generate[n] = true
	}
	current := parseEnv(env)
	res, err := in.resolve(ctx, t, nil, resolveRequest{
		values:    req.Values,
		installed: &installedValues{current: current, generate: generate},
	})
	if err != nil {
		return nil, err
	}
	next := mergeEnv(env, sortedKeys(t.Block.Inputs), res.values, current)
	if _, err := in.Stacks.UpdateEnv(ctx, name, next); err != nil {
		return nil, err
	}
	return in.configOf(ctx, st, t, parseEnv(next))
}

func (in *Installer) loadInstalled(ctx context.Context, name string) (container.Stack, *Template, string, error) {
	st, err := in.Stacks.Get(ctx, name)
	if err != nil {
		return container.Stack{}, nil, "", err
	}
	t, err := storedTemplate(st)
	if err != nil {
		return container.Stack{}, nil, "", err
	}
	env, err := in.Stacks.Env(ctx, name)
	if err != nil {
		return container.Stack{}, nil, "", err
	}
	return st, t, env, nil
}

// storedTemplate parses the template out of a stack's stored Compose text:
// only the x-hoserva block is read, so a file edited by hand is fine as long
// as the block is still valid.
func storedTemplate(st container.Stack) (*Template, error) {
	var top map[string]any
	if err := yaml.Unmarshal([]byte(st.Compose), &top); err != nil {
		return nil, fmt.Errorf("%w: %s: not valid YAML: %v", ErrInvalidTemplate, st.Name, err)
	}
	if _, ok := top[BlockKey]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrStackHasNoTemplate, st.Name)
	}
	t, issues := Parse([]byte(st.Compose))
	if t == nil {
		return nil, invalidTemplate(st.Name, issues)
	}
	return t, nil
}

func (in *Installer) configOf(ctx context.Context, st container.Stack, t *Template, current map[string]string) (*StackConfig, error) {
	var shares []string
	for _, n := range sortedKeys(t.Block.Inputs) {
		spec := t.Block.Inputs[n]
		if spec.Kind == KindPath && spec.Role != RoleAppdata && in.Shares != nil {
			var err error
			if shares, err = in.Shares(ctx); err != nil {
				return nil, fmt.Errorf("listing the shares: %w", err)
			}
			sort.Strings(shares)
			break
		}
	}
	out := &StackConfig{Stack: st}
	for _, n := range sortedKeys(t.Block.Inputs) {
		spec := t.Block.Inputs[n]
		ci := ConfigInput{Name: n, Kind: spec.Kind, Role: spec.Role, Label: spec.Label, Description: spec.Description}
		switch spec.Kind {
		case KindSecret:
			ci.Set = current[n] != ""
		case KindDevice:
			ci.Value = current[n]
			ci.ReadOnly = true
		case KindPath:
			ci.Value = current[n]
			if spec.Role != RoleAppdata {
				for _, s := range shares {
					ci.Suggestions = append(ci.Suggestions, path.Join(poolRoot, s))
				}
			}
		default:
			ci.Value = current[n]
		}
		out.Inputs = append(out.Inputs, ci)
	}
	return out, nil
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// parseEnvLine reads one line of a .env file: the variable it defines and its
// value, with the quoting Compose takes off: in double quotes the escapes
// dotenvValue writes (\", \\, \$, \n, \r); in single quotes nothing, so a
// backslash before the closing quote is read as it was written by older
// installs. A blank line, a comment and a line that defines nothing report
// false.
func parseEnvLine(line string) (key, value string, ok bool) {
	l := strings.TrimSpace(line)
	if l == "" || strings.HasPrefix(l, "#") {
		return "", "", false
	}
	if rest, found := strings.CutPrefix(l, "export"); found && (strings.HasPrefix(rest, " ") || strings.HasPrefix(rest, "\t")) {
		l = strings.TrimSpace(rest)
	}
	k, v, found := strings.Cut(l, "=")
	k = strings.TrimSpace(k)
	if !found || !envKeyPattern.MatchString(k) {
		return "", "", false
	}
	v = strings.TrimLeft(v, " \t")
	switch {
	case strings.HasPrefix(v, "'"):
		v = v[1:]
		if end := strings.Index(v, "'"); end >= 0 {
			v = v[:end]
		}
		return k, v, true
	case strings.HasPrefix(v, `"`):
		var sb strings.Builder
		for i := 1; i < len(v); i++ {
			if v[i] == '"' {
				break
			}
			if v[i] == '\\' && i+1 < len(v) {
				switch v[i+1] {
				case '"', '\\', '$':
					i++
				case 'n':
					i++
					sb.WriteByte('\n')
					continue
				case 'r':
					i++
					sb.WriteByte('\r')
					continue
				}
			}
			sb.WriteByte(v[i])
		}
		return k, sb.String(), true
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return k, strings.TrimSpace(v), true
}

// parseEnv returns the variables a .env file defines. A variable defined more
// than once has its last value, as Compose reads it.
func parseEnv(env string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(env, "\n") {
		if k, v, ok := parseEnvLine(l); ok {
			out[k] = v
		}
	}
	return out
}

// mergeEnv returns env with the inputs whose value differs from what the
// file defines (or that it does not define) written as install writes them.
// The first definition of such an input is replaced and any repeat dropped;
// a new one is appended in name order. Every other line, and every input
// that did not change, is kept as it is.
func mergeEnv(env string, names []string, values, current map[string]string) string {
	write := map[string]string{}
	for _, n := range names {
		if cur, defined := current[n]; !defined || cur != values[n] {
			write[n] = values[n]
		}
	}
	if len(write) == 0 {
		return env
	}
	var lines []string
	if env != "" {
		lines = strings.Split(strings.TrimSuffix(env, "\n"), "\n")
	}
	written := map[string]bool{}
	out := make([]string, 0, len(lines)+len(write))
	for _, l := range lines {
		k, _, ok := parseEnvLine(l)
		v, replaced := write[k]
		if !ok || !replaced {
			out = append(out, l)
			continue
		}
		if !written[k] {
			written[k] = true
			out = append(out, k+"="+dotenvValue(v))
		}
	}
	for _, n := range names {
		if v, ok := write[n]; ok && !written[n] {
			out = append(out, n+"="+dotenvValue(v))
		}
	}
	return strings.Join(out, "\n") + "\n"
}
