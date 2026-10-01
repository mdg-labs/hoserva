package template

import (
	"fmt"
	"path"
	"strings"
)

// Privilege kinds (doc 01 §7, doc 04 §7). A template never declares them:
// they are read from the Compose content.
const (
	PrivilegePrivileged       = "privileged"
	PrivilegeHostNetwork      = "host_network"
	PrivilegeHostPID          = "host_pid"
	PrivilegeHostCgroup       = "host_cgroup"
	PrivilegeDeviceCgroupRule = "device_cgroup_rules"
	PrivilegeAddedCaps        = "added_capabilities"
	PrivilegeNoConfinement    = "confinement_disabled"
	PrivilegeGroupAdd         = "group_add"
	PrivilegeDockerSocket     = "docker_socket"
	PrivilegeHostPath         = "host_path"
)

var privilegeDescriptions = map[string]string{
	PrivilegePrivileged:       "Runs with full access to the server: every device, and none of the usual isolation from the system.",
	PrivilegeHostNetwork:      "Shares the server's network directly, so it can listen on and reach every network interface and port.",
	PrivilegeHostPID:          "Shares a process namespace with the server or another container, so it can see and signal those processes.",
	PrivilegeHostCgroup:       "Shares the server's control-group view, so it can see how every process on the server is set up and limited.",
	PrivilegeDeviceCgroupRule: "Is allowed to use whole classes of devices through cgroup rules, which can reach disks and other hardware.",
	PrivilegeAddedCaps:        "Is given extra Linux capabilities, which let it do things an ordinary container is not allowed to, such as changing network settings or, with the strongest ones, administering the server.",
	PrivilegeNoConfinement:    "Switches off or replaces part of the container's security confinement (AppArmor, seccomp, SELinux labelling, no-new-privileges or hidden system paths), which weakens what keeps it away from the rest of the server.",
	PrivilegeGroupAdd:         "Joins extra groups, so it gets the access those groups have to the files and devices it can reach.",
	PrivilegeDockerSocket:     "Can control Docker itself, which is the same as full control of the server.",
	PrivilegeHostPath:         "Mounts a folder outside the storage pool and the cache, so it can read or change system files or disks.",
}

// Privilege is one thing a template's Compose content asks for beyond an
// ordinary container.
type Privilege struct {
	Kind        string
	Service     string
	Detail      string
	Description string
}

// dockerSockets are the host paths of the Docker Engine's socket; a bind
// mount of either, or of a directory that holds one, hands the container
// control of Docker.
var dockerSockets = []string{"/var/run/docker.sock", "/run/docker.sock"}

// Privileges computes the privilege summary from the Compose content with
// the install values substituted: privileged mode, host networking, the
// host PID and cgroup namespaces, device cgroup rules, added capabilities,
// disabled or replaced confinement, extra groups, the Docker socket and host
// paths outside the pool and cache, whether mounted by a volume, a named
// volume's driver options or a devices entry. sysctls are not reported (doc
// 04 §7). Services are in name order.
func (t *Template) Privileges(values map[string]string) []Privilege {
	var out []Privilege
	services := t.services()
	top, _ := t.Compose["volumes"].(map[string]any)
	for _, name := range sortedKeys(services) {
		out = append(out, servicePrivileges(name, services[name], top, values)...)
	}
	return out
}

func servicePrivileges(name string, svc, top map[string]any, values map[string]string) []Privilege {
	var out []Privilege
	add := func(kind, detail string) {
		out = append(out, Privilege{Kind: kind, Service: name, Detail: detail, Description: privilegeDescriptions[kind]})
	}
	text := func(key string) string {
		switch x := svc[key].(type) {
		case string:
			return interpolate(x, values)
		case bool:
			return fmt.Sprint(x)
		}
		return ""
	}

	if raw, present := svc["privileged"]; present {
		if on, ok := composeBool(raw, values); on || !ok {
			add(PrivilegePrivileged, "")
		}
	}
	if text("network_mode") == "host" {
		add(PrivilegeHostNetwork, "")
	}
	if pid := text("pid"); pid == "host" || strings.HasPrefix(pid, "container:") {
		add(PrivilegeHostPID, pid)
	}
	if text("cgroup") == "host" {
		add(PrivilegeHostCgroup, "")
	}
	if rules, _ := svc["device_cgroup_rules"].([]any); len(rules) > 0 {
		parts := make([]string, len(rules))
		for i, r := range rules {
			parts[i] = interpolate(fmt.Sprint(r), values)
		}
		add(PrivilegeDeviceCgroupRule, strings.Join(parts, ", "))
	}
	if caps := entries(svc["cap_add"], values); len(caps) > 0 {
		add(PrivilegeAddedCaps, strings.Join(caps, ", "))
	}
	var weakened []string
	for _, opt := range entries(svc["security_opt"], values) {
		if !confinementKept(opt) {
			weakened = append(weakened, opt)
		}
	}
	if len(weakened) > 0 {
		add(PrivilegeNoConfinement, strings.Join(weakened, ", "))
	}
	if groups := entries(svc["group_add"], values); len(groups) > 0 {
		add(PrivilegeGroupAdd, strings.Join(groups, ", "))
	}
	hostSource := func(src string) {
		exact, holds := exposesDockerSocket(src)
		if exact || holds {
			add(PrivilegeDockerSocket, src)
		}
		if !exact && !insideLayout(src) {
			add(PrivilegeHostPath, src)
		}
	}
	vols, _ := svc["volumes"].([]any)
	for _, v := range vols {
		v = interpolateVolume(v, values)
		if src, ok := bindSource(v); ok && src != "" {
			hostSource(src)
			continue
		}
		if src := namedVolumeDevice(v, top, values); src != "" {
			hostSource(src)
		}
	}
	devs, _ := svc["devices"].([]any)
	for _, d := range devs {
		if src := deviceSource(interpolateVolume(d, values)); src != "" {
			hostSource(src)
		}
	}
	return out
}

// entries reads a list-valued service key (or a lone scalar in its place)
// with the install values substituted, dropping entries that are empty after
// substitution: an empty capability, group or option grants nothing and
// Docker refuses to start with it.
func entries(v any, values map[string]string) []string {
	var raw []any
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		raw = x
	default:
		raw = []any{x}
	}
	var out []string
	for _, e := range raw {
		s := fmt.Sprint(e)
		if str, ok := e.(string); ok {
			s = interpolate(str, values)
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// confinementKept reports whether a security_opt entry leaves the container's
// confinement as strong as its defaults: no-new-privileges switched on, or a
// named AppArmor profile that the host has loaded. Everything else — an
// unconfined AppArmor or seccomp, label:disable, unmasked system paths, a
// custom seccomp profile or SELinux label the summary cannot read, and any
// option it does not know — is reported, never read as harmless.
func confinementKept(opt string) bool {
	i := strings.IndexAny(opt, ":=")
	if i < 0 {
		return strings.EqualFold(opt, "no-new-privileges")
	}
	key, val := strings.ToLower(opt[:i]), strings.TrimSpace(opt[i+1:])
	switch key {
	case "no-new-privileges":
		switch strings.ToLower(val) {
		case "true", "1", "t":
			return true
		}
	case "apparmor":
		return val != "" && !strings.EqualFold(val, "unconfined")
	}
	return false
}

// namedVolumeDevice returns the host path a named volume of the local driver
// is backed by when its driver_opts point at one. The mount call ignores the
// type when the options bind, so a bind, or a device that is a local path, is
// a host path whatever the type says; a volume of a network or memory type
// otherwise has none.
func namedVolumeDevice(v any, top map[string]any, values map[string]string) string {
	var name string
	switch x := v.(type) {
	case string:
		if parts := splitVolume(x); len(parts) >= 2 {
			name = parts[0]
		}
	case map[string]any:
		if x["type"] == "volume" {
			name, _ = x["source"].(string)
		}
	}
	vol, _ := top[name].(map[string]any)
	opts, _ := vol["driver_opts"].(map[string]any)
	device, _ := opts["device"].(string)
	device = strings.TrimSpace(interpolate(device, values))
	if device == "" {
		return ""
	}
	kind, _ := opts["type"].(string)
	options, _ := opts["o"].(string)
	if shareVolumeTypes[strings.ToLower(interpolate(kind, values))] &&
		!bindsHostPath(interpolate(options, values)) && !strings.HasPrefix(device, "/") {
		return ""
	}
	return device
}

// bindsHostPath reports whether mount options contain bind or rbind.
func bindsHostPath(options string) bool {
	for _, o := range strings.Split(options, ",") {
		switch strings.ToLower(strings.TrimSpace(o)) {
		case "bind", "rbind":
			return true
		}
	}
	return false
}

// deviceSource returns the host side of a devices entry: a short-syntax
// HOST[:CONTAINER[:PERMISSIONS]] string or a long-syntax entry with a source.
func deviceSource(d any) string {
	switch x := d.(type) {
	case string:
		src, _, _ := strings.Cut(x, ":")
		return src
	case map[string]any:
		if src, ok := x["source"].(string); ok {
			return src
		}
	}
	return fmt.Sprint(d)
}

// composeBool reads a value the way Compose reads a boolean: a YAML boolean,
// or a string that is y, yes, true or on (true) or n, no, false or off
// (false) in any case, after the install values are substituted. ok is false
// for anything else, which Compose refuses; a caller never treats that as
// false.
func composeBool(v any, values map[string]string) (on, ok bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		switch strings.ToLower(interpolate(x, values)) {
		case "y", "yes", "true", "on":
			return true, true
		case "n", "no", "false", "off":
			return false, true
		}
	}
	return false, false
}

// checkResolved refuses an install whose values make privileged something
// Compose does not read as a boolean. Check settles a value spelled out in
// the template; one that comes from an input is known only here.
func (t *Template) checkResolved(values map[string]string) error {
	services := t.services()
	for _, name := range sortedKeys(services) {
		raw, present := services[name]["privileged"]
		if !present {
			continue
		}
		if _, ok := composeBool(raw, values); !ok {
			resolved := fmt.Sprint(raw)
			if s, isStr := raw.(string); isStr {
				resolved = interpolate(s, values)
			}
			return invalid("service %s: privileged resolves to %q, which is not true or false", name, resolved)
		}
	}
	return nil
}

// interpolateVolume substitutes the install values into the parts of a
// volume entry that decide what is mounted.
func interpolateVolume(v any, values map[string]string) any {
	switch x := v.(type) {
	case string:
		return interpolate(x, values)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if s, ok := val.(string); ok && (k == "source" || k == "type") {
				val = interpolate(s, values)
			}
			out[k] = val
		}
		return out
	}
	return v
}

// insideLayout reports whether a bind mount's host side lies in the pool
// or on the cache, the two places Hoserva's own layout puts data (D10). A
// path relative to the stack directory is outside both.
func insideLayout(src string) bool {
	if !strings.HasPrefix(src, "/") {
		return false
	}
	p := path.Clean(src)
	return under(p, poolRoot) || under(p, cacheRoot)
}

// exposesDockerSocket reports whether a bind mount source is the Docker
// socket itself (exact) or a directory that holds it.
func exposesDockerSocket(src string) (exact, holds bool) {
	if !strings.HasPrefix(src, "/") {
		return false, false
	}
	p := path.Clean(src)
	for _, sock := range dockerSockets {
		switch {
		case p == sock:
			exact = true
		case p == "/" || strings.HasPrefix(sock, p+"/"):
			holds = true
		}
	}
	return exact, holds
}

// interpolate substitutes values into s the way Compose does: $NAME,
// ${NAME}, ${NAME:-default}, ${NAME-default}, ${NAME:+alt}, ${NAME+alt} and
// the $$ escape. A name with no value is empty.
func interpolate(s string, values map[string]string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			sb.WriteByte(s[i])
			continue
		}
		rest := s[i+1:]
		switch {
		case strings.HasPrefix(rest, "$"):
			sb.WriteByte('$')
			i++
		case strings.HasPrefix(rest, "{"):
			name := variableName(rest[1:])
			if name == "" {
				sb.WriteByte('$')
				continue
			}
			body := rest[1+len(name):]
			end := closingBrace(body)
			sb.WriteString(expand(name, body[:end], values))
			i += 2 + len(name) + end
		default:
			name := variableName(rest)
			if name == "" {
				sb.WriteByte('$')
				continue
			}
			sb.WriteString(values[name])
			i += len(name)
		}
	}
	return sb.String()
}

// expand applies the operator that follows a variable name inside ${...}.
func expand(name, op string, values map[string]string) string {
	val, set := values[name]
	switch {
	case op == "":
		return val
	case strings.HasPrefix(op, ":-"):
		if val == "" {
			return interpolate(op[2:], values)
		}
	case strings.HasPrefix(op, "-"):
		if !set {
			return interpolate(op[1:], values)
		}
	case strings.HasPrefix(op, ":+"):
		if val != "" {
			return interpolate(op[2:], values)
		}
		return ""
	case strings.HasPrefix(op, "+"):
		if set {
			return interpolate(op[1:], values)
		}
		return ""
	}
	return val
}
