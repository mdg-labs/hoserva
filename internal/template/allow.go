package template

import (
	"fmt"
	"path"
	"strings"
)

// A template is accepted only when every Compose key in it is one whose effect
// on the host is nil or is read by the privilege summary. A key outside these
// lists is refused rather than guessed at, so a Compose feature the summary
// does not know about can never be used to understate what a template asks for.

var topLevelKeys = setOf("services", "volumes", "networks", "name")

// serviceKeys: the first group reaches nothing on the host beyond the
// container itself (sysctls are the namespaced ones Docker applies inside the
// container's own namespaces and refuses in a host one); the second is read by
// the privilege summary or checked below.
var serviceKeys = setOf(
	"image", "command", "entrypoint", "environment", "restart", "healthcheck", "labels",
	"user", "working_dir", "container_name", "hostname", "domainname", "depends_on", "links",
	"logging", "stop_signal", "stop_grace_period", "tty", "stdin_open", "init", "read_only",
	"tmpfs", "ulimits", "mem_limit", "memswap_limit", "mem_reservation", "cpus", "cpuset",
	"cpu_shares", "pids_limit", "shm_size", "expose", "ports", "networks", "dns", "dns_search",
	"extra_hosts", "sysctls", "cap_drop", "runtime",
	"platform", "pull_policy", "ipc", "deploy",

	"privileged", "network_mode", "pid", "cgroup", "device_cgroup_rules", "devices", "volumes",
	"volumes_from", "extends", "env_file", "cap_add", "security_opt", "group_add",
)

var (
	deployKeys          = setOf("resources", "restart_policy", "replicas", "labels")
	deployResourceKeys  = setOf("limits", "reservations")
	volumeDefinitionKey = setOf("driver", "driver_opts", "labels")
	volumeOptionKeys    = setOf("type", "o", "device")
	networkKeys         = setOf("driver", "driver_opts", "ipam", "external", "name", "internal", "attachable", "labels", "enable_ipv6", "enable_ipv4")
	mountTypes          = setOf("bind", "volume", "tmpfs")
	ipcModes            = setOf("private", "shareable", "none")

	// Local-driver volume types that mount a network share or memory. They
	// reach no host path unless the options bind one (see bindsHostPath) or
	// the device is a local path.
	shareVolumeTypes = setOf("nfs", "nfs4", "cifs", "smb3", "tmpfs")
	// Local-driver volume types whose device is a host path; the summary
	// reports it.
	deviceVolumeTypes = setOf("none", "ext2", "ext3", "ext4", "xfs", "btrfs")
)

var refusedKeyReasons = map[string]string{
	"include": "include pulls in services from another file, so their privileges cannot be computed; spell the services out in the template",
	"secrets": "secrets and configs mount a file of the host into the container; use an input of kind secret instead",
	"configs": "secrets and configs mount a file of the host into the container; use an input of kind secret instead",
	"build":   "build reads a host directory into an image; a template ships a pinned image instead",
}

func setOf(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func isExtension(key string) bool { return strings.HasPrefix(key, "x-") }

func refusedKey(key string) string {
	if reason := refusedKeyReasons[key]; reason != "" {
		return reason
	}
	return fmt.Sprintf("%q is not accepted in a template: only Compose keys whose effect on the host the privilege summary classifies are", key)
}

// literal returns the string a value spells out, and false when it is not a
// string or is interpolated, so its meaning depends on an install value.
func literal(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && !strings.Contains(s, "$")
}

func checkAllowedKeys(t *Template) []Issue {
	var out []Issue
	for _, k := range sortedKeys(t.Compose) {
		if k != BlockKey && !isExtension(k) && !topLevelKeys[k] {
			out = append(out, Issue{Path: []string{k}, Message: refusedKey(k)})
		}
	}
	rawServices, _ := t.Compose["services"].(map[string]any)
	services := t.services()
	for _, name := range sortedKeys(rawServices) {
		svc, ok := services[name]
		if !ok {
			out = append(out, Issue{Path: []string{"services", name}, Message: "must be a mapping"})
			continue
		}
		out = append(out, checkServiceKeys(name, svc, services)...)
	}
	out = append(out, checkVolumeDefinitions(t.Compose["volumes"])...)
	out = append(out, checkNetworkDefinitions(t.Compose["networks"])...)
	return out
}

func checkServiceKeys(name string, svc map[string]any, services map[string]map[string]any) []Issue {
	var out []Issue
	at := func(rest ...string) []string { return append([]string{"services", name}, rest...) }
	refuse := func(p []string, msg string) { out = append(out, Issue{Path: p, Message: msg}) }

	for _, k := range sortedKeys(svc) {
		if !serviceKeys[k] && !isExtension(k) {
			refuse(at(k), refusedKey(k))
		}
	}

	if v, ok := svc["privileged"]; ok {
		if s, isStr := v.(string); !isStr || !strings.Contains(s, "$") {
			if _, ok := composeBool(v, nil); !ok {
				refuse(at("privileged"), "must be true or false, or an input substituted with ${...}: any other value is refused by Compose or read as a boolean in a way the privilege summary cannot rely on")
			}
		}
	}
	if v, ok := svc["pull_policy"]; ok {
		if s, lit := literal(v); !lit || s == "build" {
			refuse(at("pull_policy"), "must be spelled out and must not be build, which would build an image from a host directory")
		}
	}
	if v, ok := svc["ipc"]; ok {
		s, lit := literal(v)
		if ref, isService := strings.CutPrefix(s, "service:"); isService {
			_, own := services[ref]
			lit = lit && own
		} else {
			lit = lit && ipcModes[s]
		}
		if !lit {
			refuse(at("ipc"), "must be private, shareable, none or a service of this template: any other mode shares the memory of the host or of a container outside the template")
		}
	}
	if v, ok := svc["network_mode"]; ok {
		s, lit := literal(v)
		ref, isService := strings.CutPrefix(s, "service:")
		_, own := services[ref]
		if !lit || strings.HasPrefix(s, "container:") || isService && !own {
			refuse(at("network_mode"), "must be spelled out, and may share the network only of a service of this template")
		}
	}
	if v, ok := svc["env_file"]; ok {
		out = append(out, checkEnvFile(at("env_file"), v)...)
	}
	if v := svc["volumes"]; v != nil {
		entries, isList := v.([]any)
		if !isList {
			refuse(at("volumes"), "must be a list")
		}
		for i, e := range entries {
			switch x := e.(type) {
			case string:
			case map[string]any:
				if s, lit := literal(x["type"]); !lit || !mountTypes[s] {
					refuse(at("volumes", fmt.Sprint(i), "type"), "must be spelled out as bind, volume or tmpfs")
				}
			default:
				refuse(at("volumes", fmt.Sprint(i)), "must be a volume string or a mapping")
			}
		}
	}
	for _, n := range networkNames(svc["networks"]) {
		if n == "host" {
			refuse(at("networks"), "attaches to the host network, which is host networking; set network_mode: host so it is reported")
		}
	}
	if v, ok := svc["deploy"]; ok {
		deploy, isMap := v.(map[string]any)
		if !isMap {
			refuse(at("deploy"), "must be a mapping")
		}
		for _, k := range sortedKeys(deploy) {
			if !deployKeys[k] && !isExtension(k) {
				refuse(at("deploy", k), fmt.Sprintf("%q is not accepted in a template: only deploy resources, restart_policy, replicas and labels are", k))
			}
		}
		resources, _ := deploy["resources"].(map[string]any)
		for _, k := range sortedKeys(resources) {
			if !deployResourceKeys[k] {
				refuse(at("deploy", "resources", k), fmt.Sprintf("%q is not accepted in a template: only limits and reservations are", k))
			}
		}
	}
	return out
}

// checkEnvFile accepts only the stack's own .env, which Install writes: any
// other file would be read from the host.
func checkEnvFile(p []string, v any) []Issue {
	var out []Issue
	own := func(p []string, e any) {
		if m, ok := e.(map[string]any); ok {
			e = m["path"]
		}
		if s, ok := e.(string); !ok || path.Clean(s) != ".env" {
			out = append(out, Issue{Path: p, Message: "only the stack's own .env can be loaded; any other file would be read from the host"})
		}
	}
	if list, ok := v.([]any); ok {
		for i, e := range list {
			own(append(p[:len(p):len(p)], fmt.Sprint(i)), e)
		}
		return out
	}
	own(p, v)
	return out
}

func networkNames(v any) []string {
	switch x := v.(type) {
	case map[string]any:
		return sortedKeys(x)
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// checkVolumeDefinitions accepts a named volume of the local driver whose
// options either mount a share or memory, or name a device that the privilege
// summary reports; any other driver, an external volume or a volume with
// another name could reach what the summary cannot see.
func checkVolumeDefinitions(v any) []Issue {
	if v == nil {
		return nil
	}
	volumes, ok := v.(map[string]any)
	if !ok {
		return []Issue{{Path: []string{"volumes"}, Message: "must be a mapping"}}
	}
	var out []Issue
	for _, name := range sortedKeys(volumes) {
		at := func(rest ...string) []string { return append([]string{"volumes", name}, rest...) }
		refuse := func(p []string, msg string) { out = append(out, Issue{Path: p, Message: msg}) }
		if volumes[name] == nil {
			continue
		}
		def, ok := volumes[name].(map[string]any)
		if !ok {
			refuse(at(), "must be a mapping")
			continue
		}
		for _, k := range sortedKeys(def) {
			if !volumeDefinitionKey[k] && !isExtension(k) {
				refuse(at(k), fmt.Sprintf("%q is not accepted in a named volume: only driver, driver_opts and labels are, since another name or an external volume can be another stack's data", k))
			}
		}
		if d, ok := def["driver"]; ok {
			if s, lit := literal(d); !lit || s != "local" {
				refuse(at("driver"), "must be local: another volume driver can reach what the privilege summary cannot see")
			}
		}
		opts, hasOpts := def["driver_opts"]
		if !hasOpts || opts == nil {
			continue
		}
		optMap, isMap := opts.(map[string]any)
		if !isMap {
			refuse(at("driver_opts"), "must be a mapping")
			continue
		}
		for _, k := range sortedKeys(optMap) {
			if _, isString := optMap[k].(string); !volumeOptionKeys[k] || !isString {
				refuse(at("driver_opts", k), "only the string options type, o and device are accepted")
			}
		}
		if typ, ok := optMap["type"]; ok {
			s, lit := literal(typ)
			s = strings.ToLower(s)
			if !lit || !shareVolumeTypes[s] && !deviceVolumeTypes[s] {
				refuse(at("driver_opts", "type"), "must be spelled out as nfs, nfs4, cifs, smb3, tmpfs, none, ext2, ext3, ext4, xfs or btrfs: another file system type can take host paths from its options")
			}
		}
	}
	return out
}

// checkNetworkDefinitions refuses a network that is the host's own, and an
// external that is not a plain boolean: the summary reports host networking
// only through network_mode.
func checkNetworkDefinitions(v any) []Issue {
	if v == nil {
		return nil
	}
	networks, ok := v.(map[string]any)
	if !ok {
		return []Issue{{Path: []string{"networks"}, Message: "must be a mapping"}}
	}
	var out []Issue
	for _, name := range sortedKeys(networks) {
		at := func(rest ...string) []string { return append([]string{"networks", name}, rest...) }
		refuse := func(p []string, msg string) { out = append(out, Issue{Path: p, Message: msg}) }
		if name == "host" {
			refuse(at(), "is the host network, which is host networking; set network_mode: host so it is reported")
		}
		if networks[name] == nil {
			continue
		}
		def, ok := networks[name].(map[string]any)
		if !ok {
			refuse(at(), "must be a mapping")
			continue
		}
		for _, k := range sortedKeys(def) {
			if !networkKeys[k] && !isExtension(k) {
				refuse(at(k), fmt.Sprintf("%q is not accepted in a network definition", k))
			}
		}
		if v, ok := def["external"]; ok {
			if _, isBool := v.(bool); !isBool {
				refuse(at("external"), "must be true or false: the older external mapping names the network, which could be the host's")
			}
		}
		for _, k := range []string{"name", "driver"} {
			if v, ok := def[k]; ok {
				if s, lit := literal(v); !lit || s == "host" {
					refuse(at(k), "must be spelled out and must not be host, which is host networking; set network_mode: host so it is reported")
				}
			}
		}
	}
	return out
}
