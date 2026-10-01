package template

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Host paths of the layout a template's defaults follow (doc 01 §6, D10).
const (
	cacheRoot = "/mnt/cache"
	poolRoot  = "/mnt/user"
)

// A Check inspects a schema-valid template for a rule the schema cannot
// express. Each one is independent, and further checks (the privilege audit)
// are added to the checks list without changing the others.
type Check func(t *Template) []Issue

var checks = []Check{
	checkServices,
	checkReferences,
	checkPathDefaults,
	checkBindSources,
	checkUserIDs,
}

// Check runs every rule beyond the schema and returns what it found, in a
// stable order.
func (t *Template) Check() []Issue {
	var out []Issue
	for _, c := range checks {
		out = append(out, c(t)...)
	}
	for i := range out {
		out[i].Line = lineOf(t.root, out[i].Path)
	}
	return out
}

func (t *Template) services() map[string]map[string]any {
	raw, _ := t.Compose["services"].(map[string]any)
	out := make(map[string]map[string]any, len(raw))
	for name, v := range raw {
		if svc, ok := v.(map[string]any); ok {
			out[name] = svc
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func checkServices(t *Template) []Issue {
	if len(t.services()) == 0 {
		return []Issue{{Path: []string{"services"}, Message: "a template needs at least one service"}}
	}
	return nil
}

// references lists the variables Compose would interpolate in s: $NAME,
// ${NAME} and ${NAME:-default}, where the default may itself interpolate.
// $$ is the escape for a literal dollar sign.
func references(s string) []string {
	var out []string
	scanReferences(s, &out)
	return out
}

func scanReferences(s string, out *[]string) {
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			continue
		}
		rest := s[i+1:]
		switch {
		case strings.HasPrefix(rest, "$"):
			i++
		case strings.HasPrefix(rest, "{"):
			name := variableName(rest[1:])
			if name == "" {
				continue
			}
			*out = append(*out, name)
			body := rest[1+len(name):]
			end := closingBrace(body)
			scanReferences(body[:end], out)
			i += 2 + len(name) + end
		default:
			if name := variableName(rest); name != "" {
				*out = append(*out, name)
				i += len(name)
			}
		}
	}
}

// variableName returns the leading [A-Za-z_][A-Za-z0-9_]* of s, or "".
func variableName(s string) string {
	n := 0
	for n < len(s) {
		c := s[n]
		if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || n > 0 && c >= '0' && c <= '9' {
			n++
			continue
		}
		break
	}
	return s[:n]
}

// closingBrace returns the index of the brace that closes a ${ whose
// contents start at body[0], or len(body) when it is never closed.
func closingBrace(body string) int {
	depth := 0
	for j := 0; j < len(body); j++ {
		switch {
		case strings.HasPrefix(body[j:], "$$"):
			j++
		case strings.HasPrefix(body[j:], "${"):
			depth++
			j++
		case body[j] == '}':
			if depth == 0 {
				return j
			}
			depth--
		}
	}
	return len(body)
}

// walkStrings calls fn for every string in v, with its path.
func walkStrings(v any, p []string, fn func(p []string, s string)) {
	switch x := v.(type) {
	case string:
		fn(p, x)
	case map[string]any:
		for _, k := range sortedKeys(x) {
			walkStrings(x[k], append(p[:len(p):len(p)], k), fn)
		}
	case []any:
		for i, e := range x {
			walkStrings(e, append(p[:len(p):len(p)], fmt.Sprint(i)), fn)
		}
	}
}

// checkReferences holds every ${VAR} in the Compose file and the web UI
// address to a declared input, and every declared input to a use.
func checkReferences(t *Template) []Issue {
	var out []Issue
	used := map[string]bool{}
	note := func(p []string, s string) {
		for _, name := range references(s) {
			used[name] = true
			if _, ok := t.Block.Inputs[name]; !ok {
				out = append(out, Issue{Path: p, Message: fmt.Sprintf("references ${%s}, which is not declared in x-hoserva.inputs", name)})
			}
		}
	}
	for _, k := range sortedKeys(t.Compose) {
		if k == BlockKey {
			continue
		}
		walkStrings(t.Compose[k], []string{k}, note)
	}
	note([]string{BlockKey, "webui"}, t.Block.WebUI)
	// Install writes every input to the stack's .env, so a service that
	// loads it receives them all (doc 04 §2).
	allUsed := false
	for _, svc := range t.services() {
		allUsed = allUsed || loadsStackEnv(svc["env_file"])
	}
	for _, name := range sortedKeys(t.Block.Inputs) {
		if t.Block.Inputs[name].Kind != KindDevice && !used[name] && !allUsed {
			out = append(out, Issue{Path: []string{BlockKey, "inputs", name}, Message: "is declared but nothing in the template uses it"})
		}
	}
	return out
}

// loadsStackEnv reports whether an env_file value, in any of Compose's
// forms, names the stack's own .env and no other file counts.
func loadsStackEnv(v any) bool {
	isStackEnv := func(p any) bool {
		s, ok := p.(string)
		return ok && path.Clean(s) == ".env"
	}
	switch x := v.(type) {
	case string:
		return isStackEnv(x)
	case []any:
		for _, e := range x {
			if m, ok := e.(map[string]any); ok {
				e = m["path"]
			}
			if isStackEnv(e) {
				return true
			}
		}
	}
	return false
}

// checkPathDefaults keeps appdata on cache and media on the pool (doc 04 §7).
func checkPathDefaults(t *Template) []Issue {
	var out []Issue
	for _, name := range sortedKeys(t.Block.Inputs) {
		in := t.Block.Inputs[name]
		if in.Kind != KindPath {
			continue
		}
		p := []string{BlockKey, "inputs", name}
		def, _ := in.Default.(string)
		switch in.Role {
		case RoleAppdata:
			if def == "" {
				out = append(out, Issue{Path: p, Message: "an appdata input needs a default under " + cacheRoot})
			} else if !under(def, cacheRoot) {
				out = append(out, Issue{Path: append(p, "default"), Message: fmt.Sprintf("appdata belongs on cache: %q is not under %s", def, cacheRoot)})
			}
		case RoleShare, RoleMedia, RoleDownloads:
			if def != "" && !under(def, poolRoot) {
				out = append(out, Issue{Path: append(p, "default"), Message: fmt.Sprintf("%s belongs on the pool: %q is not under %s", in.Role, def, poolRoot)})
			}
		}
	}
	return out
}

func under(p, root string) bool {
	p = path.Clean(p)
	return p == root || strings.HasPrefix(p, root+"/")
}

// checkBindSources requires a bind mount's host side to be a path input, or
// an absolute path the template spells out, never a path relative to the
// stack directory and never another kind of input.
func checkBindSources(t *Template) []Issue {
	var out []Issue
	services := t.services()
	for _, svcName := range sortedKeys(services) {
		vols, _ := services[svcName]["volumes"].([]any)
		for i, v := range vols {
			src, ok := bindSource(v)
			if !ok {
				continue
			}
			p := []string{"services", svcName, "volumes", fmt.Sprint(i)}
			if strings.HasPrefix(src, ".") || strings.HasPrefix(src, "~") {
				out = append(out, Issue{Path: p, Message: fmt.Sprintf("bind mount source %q is relative; use a path input so the data lands where the install form says", src)})
				continue
			}
			refs := references(src)
			if !strings.HasPrefix(src, "$") || len(refs) == 0 {
				continue
			}
			if in, declared := t.Block.Inputs[refs[0]]; declared && in.Kind != KindPath {
				out = append(out, Issue{Path: p, Message: fmt.Sprintf("bind mount source starts with ${%s}, which is a %s input, not a path input", refs[0], in.Kind)})
			}
		}
	}
	return out
}

// bindSource returns the host side of a volume entry when it is a bind
// mount: a short-syntax path source, or a long-syntax entry of type bind.
func bindSource(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		parts := splitVolume(x)
		if len(parts) < 2 {
			return "", false
		}
		src := parts[0]
		if strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") || strings.HasPrefix(src, "~") || strings.HasPrefix(src, "$") {
			return src, true
		}
	case map[string]any:
		if x["type"] == "bind" {
			src, _ := x["source"].(string)
			return src, src != ""
		}
	}
	return "", false
}

// splitVolume splits a short-syntax volume on the colons outside ${...}.
func splitVolume(s string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], "${"):
			depth++
			i++
		case s[i] == '}' && depth > 0:
			depth--
		case s[i] == ':' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// checkUserIDs holds PUID and PGID to the UID/GID model (Q26).
func checkUserIDs(t *Template) []Issue {
	want := map[string]string{"PUID": "99", "PGID": "100"}
	var out []Issue
	services := t.services()
	for _, svcName := range sortedKeys(services) {
		env := environment(services[svcName]["environment"])
		for _, key := range []string{"PUID", "PGID"} {
			if got, ok := env[key]; ok && got != want[key] {
				out = append(out, Issue{
					Path:    []string{"services", svcName, "environment", key},
					Message: fmt.Sprintf("must be %q (Q26), got %q", want[key], got),
				})
			}
		}
	}
	return out
}

// environment reads a service's environment in either Compose form.
func environment(v any) map[string]string {
	out := map[string]string{}
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			out[k] = fmt.Sprint(val)
		}
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				k, val, _ := strings.Cut(s, "=")
				out[k] = val
			}
		}
	}
	return out
}
