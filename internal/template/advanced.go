package template

import (
	"context"
	"fmt"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mdg-labs/hoserva/internal/container"
)

// Bounds of the resource limits an install accepts. Docker refuses a memory
// limit under 6 MiB; the upper bounds only keep a typo out of the Compose
// file.
const (
	minMemoryMiB   = 6
	maxMemoryMiB   = 16 << 20
	minCPUs        = 0.01
	maxCPUs        = 1024
	maxExtraParams = 4096
	maxNetworkName = 64

	networkBridge = "bridge"
	networkHost   = "host"

	advancedNetwork = "networkMode"
	advancedRestart = "restart"
	advancedCPUs    = "cpus"
	advancedMemory  = "memoryMiB"
	advancedExtra   = "extraParams"
)

// Advanced are the container settings an install takes beside a template's
// inputs (doc 03 §5.4, doc 04 §5). Each is optional: the zero value leaves
// the template's own Compose file as it is.
type Advanced struct {
	// NetworkMode is bridge, host, or the name of an existing Docker network
	// (Q37). It replaces the service's network_mode and networks.
	NetworkMode string
	// Restart is no, always, unless-stopped or on-failure.
	Restart string
	// CPUs and MemoryMiB are the service's limits; nil is no limit, and a
	// limit that is given has to be in range, zero included.
	CPUs      *float64
	MemoryMiB *int
	// ExtraParams is a docker run flag string, read as doc 04 §5 reads an
	// Unraid template's ExtraParams: parsed into Compose fields, never run.
	ExtraParams string
}

// NetworkSource lists the Docker networks that exist.
type NetworkSource interface {
	Networks(ctx context.Context) ([]container.Network, error)
}

var (
	networkNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]+$`)
	restartPolicies = map[string]bool{"no": true, "always": true, "unless-stopped": true, "on-failure": true}
)

func (a Advanced) zero() bool {
	return a == Advanced{}
}

func (a Advanced) validate() error {
	if a.NetworkMode != "" && a.NetworkMode != networkBridge && a.NetworkMode != networkHost {
		if len(a.NetworkMode) > maxNetworkName || !networkNameRe.MatchString(a.NetworkMode) {
			return invalidInput(advancedNetwork, "%q is not a Docker network name: it starts with a letter or digit, holds only letters, digits, _, . and -, and has at least two characters", a.NetworkMode)
		}
		if a.NetworkMode == "none" {
			return invalidInput(advancedNetwork, "the network none gives the container no network at all and is not offered: choose bridge, host or an existing network")
		}
	}
	if a.Restart != "" && !restartPolicies[a.Restart] {
		return invalidInput(advancedRestart, "%q is not a restart policy: use no, always, unless-stopped or on-failure", a.Restart)
	}
	if a.CPUs != nil && (math.IsNaN(*a.CPUs) || *a.CPUs < minCPUs || *a.CPUs > maxCPUs) {
		return invalidInput(advancedCPUs, "the CPU limit must be from %v to %d", minCPUs, maxCPUs)
	}
	if a.MemoryMiB != nil && (*a.MemoryMiB < minMemoryMiB || *a.MemoryMiB > maxMemoryMiB) {
		return invalidInput(advancedMemory, "the memory limit must be from %d MiB to %d MiB", minMemoryMiB, maxMemoryMiB)
	}
	if len(a.ExtraParams) > maxExtraParams || strings.ContainsRune(a.ExtraParams, 0) {
		return invalidInput(advancedExtra, "the extra parameters must be text of at most %d bytes", maxExtraParams)
	}
	return nil
}

// serviceSetting names the first setting that belongs to one service, or "".
// A template with several services has no service to give it to.
func (a Advanced) serviceSetting() string {
	switch {
	case a.NetworkMode != "":
		return advancedNetwork
	case a.CPUs != nil:
		return advancedCPUs
	case a.MemoryMiB != nil:
		return advancedMemory
	case a.ExtraParams != "":
		return advancedExtra
	}
	return ""
}

// advancedPlan is a template's Compose text with the advanced settings
// applied, the template the privilege summary is read from, and what the
// settings could not carry out.
type advancedPlan struct {
	compose  []byte
	tmpl     *Template
	warnings []Warning
	// available is false for a template with several services.
	available bool
	// ownPorts and extraPorts are the published ports, in Compose's short
	// syntax with the template's variables unresolved, that the template's
	// service publishes and that the extra parameters add to it. Both are
	// empty when the service uses the host's network, where Docker ignores
	// published ports.
	ownPorts, extraPorts []string
}

// applyAdvanced writes the settings into the template's Compose text. The
// network mode, the limits and the extra parameters go to the template's one
// service and a template with several refuses them, because no input names
// which service they mean; the restart policy goes to every service. With no
// settings the text is returned byte for byte. A named network that does not
// exist is a missing_network warning with the command that creates it, and an
// install refuses it: Hoserva never creates networks (Q37).
func (in *Installer) applyAdvanced(ctx context.Context, t *Template, data []byte, adv Advanced, install bool) (*advancedPlan, error) {
	out := &advancedPlan{compose: data, tmpl: t, available: len(t.services()) == 1}
	if err := adv.validate(); err != nil {
		return nil, err
	}
	if adv.zero() {
		return out, nil
	}
	if setting := adv.serviceSetting(); setting != "" && !out.available {
		return nil, invalidInput(setting, "this template has several services, so this setting cannot say which one it is for: change the Compose file after installing instead")
	}
	doc, services, err := serviceNodes(data)
	if err != nil {
		return nil, err
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		if services.Content[i+1].Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%w: service %s is not a mapping", ErrInvalidTemplate, services.Content[i].Value)
		}
	}

	m := &serviceMerge{}
	var ownPorts []string
	if len(services.Content) == 2 {
		svc := services.Content[1]
		ownPorts = portSpecs(svc)
		if adv.ExtraParams != "" {
			if err := m.extraParams(doc.Content[0], svc, services.Content[0], adv); err != nil {
				return nil, err
			}
		}
		if adv.MemoryMiB != nil {
			setScalar(svc, "mem_limit", stringNode(strconv.Itoa(*adv.MemoryMiB)+"m"))
		}
		if adv.CPUs != nil {
			setScalar(svc, "cpus", numberNode(strconv.FormatFloat(*adv.CPUs, 'f', -1, 64)))
		}
		if adv.NetworkMode != "" {
			if err := in.setNetwork(ctx, doc.Content[0], svc, adv.NetworkMode, install, m); err != nil {
				return nil, err
			}
		}
	}
	if adv.Restart != "" {
		policy := stringNode(adv.Restart)
		if adv.Restart == "no" {
			// Unquoted, YAML 1.1 readers take no for false.
			policy.Style = yaml.DoubleQuotedStyle
		}
		for i := 1; i < len(services.Content); i += 2 {
			setScalar(services.Content[i], "restart", policy)
		}
	}

	text, err := encodeCompose(doc)
	if err != nil {
		return nil, err
	}
	parsed, issues := Parse(text)
	if parsed == nil {
		return nil, fmt.Errorf("%w: the settings made the Compose file invalid: %s", ErrInvalidTemplate, issues[0])
	}
	out.compose, out.tmpl, out.warnings = text, parsed, m.warnings
	if len(services.Content) == 2 {
		svc := services.Content[1]
		if network := mappingValue(svc, "network_mode"); network == nil || network.Value != networkHost {
			all := portSpecs(svc)
			out.ownPorts, out.extraPorts = all[:len(ownPorts)], all[len(ownPorts):]
		}
	}
	return out, nil
}

// portSpecs lists a service's published ports in Compose's short syntax, as
// written: a long-syntax entry becomes published:target/protocol, and one
// that publishes no host port is left out.
func portSpecs(svc *yaml.Node) []string {
	ports := mappingValue(svc, "ports")
	if ports == nil || ports.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, item := range ports.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			out = append(out, item.Value)
		case yaml.MappingNode:
			published, target := mappingValue(item, "published"), mappingValue(item, "target")
			if published == nil || target == nil {
				continue
			}
			proto := "tcp"
			if p := mappingValue(item, "protocol"); p != nil {
				proto = p.Value
			}
			out = append(out, published.Value+":"+target.Value+"/"+proto)
		}
	}
	return out
}

// hostPort is a host port a service publishes with the protocol it is
// published for.
type hostPort struct {
	port  int
	proto string
}

// hostPorts reads the host ports of short-syntax port specs after the
// install values are substituted. A spec that publishes no host port, or that
// is not a port spec, adds none.
func hostPorts(specs []string, values map[string]string) []hostPort {
	var out []hostPort
	for _, spec := range specs {
		p, ok := parsePublish(interpolate(spec, values))
		if !ok || p.host == "" {
			continue
		}
		lo, hi, _ := strings.Cut(p.host, "-")
		first, _ := strconv.Atoi(lo)
		last := first
		if hi != "" {
			last, _ = strconv.Atoi(hi)
		}
		for n := first; n <= last; n++ {
			out = append(out, hostPort{n, p.proto})
		}
	}
	return out
}

// checkExtraPorts refuses host ports the extra parameters publish that the
// template's own ports, an earlier extra port, a container, a stack or the
// host already holds, the way a port input's conflict is refused: the port is
// never moved. A port that cannot be checked refuses too.
func (in *Installer) checkExtraPorts(ctx context.Context, adv *advancedPlan, values map[string]string) error {
	extra := hostPorts(adv.extraPorts, values)
	if len(extra) == 0 {
		return nil
	}
	used, err := in.usedPorts(ctx)
	if err != nil {
		return fmt.Errorf("checking the ports for conflicts: %w", err)
	}
	held := map[hostPort]bool{}
	for _, p := range hostPorts(adv.ownPorts, values) {
		held[p] = true
	}
	for _, p := range extra {
		switch {
		case held[p]:
			return &InputError{Input: advancedExtra, Kind: ErrPortTaken, Message: fmt.Sprintf("the extra parameters publish port %d/%s, which the template or an earlier extra parameter already publishes", p.port, p.proto)}
		case used[p.port]:
			return &InputError{Input: advancedExtra, Kind: ErrPortTaken, Message: fmt.Sprintf("the extra parameters publish port %d, which a container, a stack or the host already uses", p.port)}
		}
		held[p] = true
	}
	return nil
}

// setNetwork replaces the service's network settings. A network that does not
// exist is reported, and refused by an install.
func (in *Installer) setNetwork(ctx context.Context, root, svc *yaml.Node, mode string, install bool, m *serviceMerge) error {
	removeKey(svc, "network_mode")
	removeKey(svc, "networks")
	switch mode {
	case networkBridge, networkHost:
		setScalar(svc, "network_mode", stringNode(mode))
		if mode == networkHost {
			if ports := mappingValue(svc, "ports"); ports != nil && len(ports.Content) > 0 {
				m.note("The container uses the server's network, where Docker ignores published ports, so the template's ports are not published.")
			}
		}
		return nil
	}
	if in.Networks == nil {
		return fmt.Errorf("checking the Docker networks: %w: no container provider", container.ErrUnavailable)
	}
	networks, err := in.Networks.Networks(ctx)
	if err != nil {
		return fmt.Errorf("listing the Docker networks: %w", err)
	}
	exists := false
	for _, n := range networks {
		exists = exists || n.Name == mode
	}
	if !exists {
		cmd := "docker network create " + mode
		if install {
			return fmt.Errorf("%w: the network %s does not exist and Hoserva does not create networks; create it with `%s` and install again", ErrNetworkMissing, mode, cmd)
		}
		m.warnings = append(m.warnings, Warning{
			Class:   WarnMissingNetwork,
			Message: fmt.Sprintf("The network %s does not exist, and Hoserva does not create networks. Create it with the command below before installing; the install is refused until it exists.", mode),
			Detail:  mode,
			Command: cmd,
		})
	}
	setMapping(svc, "networks", mode, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
	top := mappingValue(root, "networks")
	if top == nil || top.Kind != yaml.MappingNode {
		top = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		replaceKey(root, "networks", top)
	}
	external := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		stringNode("external"), {Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
	}}
	replaceKey(top, mode, external)
	return nil
}

// serviceMerge collects what merging the extra parameters into a service
// said about it.
type serviceMerge struct {
	warnings []Warning
}

func (m *serviceMerge) warn(class, message, detail string) {
	m.warnings = append(m.warnings, Warning{Class: class, Message: message, Detail: detail})
}

func (m *serviceMerge) note(format string, args ...any) {
	m.warn(WarnNote, fmt.Sprintf(format, args...), "")
}

// extraParams reads the docker run flags the way the converter reads an Unraid
// template's ExtraParams and merges the Compose fields they give into the
// service. What cannot be translated is a warning and a comment above the
// service, never dropped silently and never passed to a shell.
func (m *serviceMerge) extraParams(root, svc, key *yaml.Node, adv Advanced) error {
	c := newConverter(ConvertOptions{})
	c.extraParams(adv.ExtraParams)
	frag, err := c.fragment()
	if err != nil {
		return err
	}
	for _, own := range []struct {
		field, setting string
		set            bool
	}{
		{"restart", advancedRestart, adv.Restart != ""},
		{"mem_limit", advancedMemory, adv.MemoryMiB != nil},
		{"cpus", advancedCPUs, adv.CPUs != nil},
	} {
		if own.set && mappingValue(frag, own.field) != nil {
			return invalidInput(advancedExtra, "the extra parameters set %s, which the %s setting sets too: use one of them", own.field, own.setting)
		}
	}
	m.warnings = append(m.warnings, c.warnings...)
	m.mapping(svc, frag, "")
	declareVolumes(root, c.namedVolumes)
	if len(c.untranslated) > 0 {
		lines := []string{"# Hoserva: could not translate the following extra parameters:"}
		for _, u := range c.untranslated {
			lines = append(lines, "#   "+u)
		}
		lines = append(lines, "# Review and add the Compose equivalent manually if required.")
		comment := strings.Join(lines, "\n")
		if key.HeadComment != "" {
			comment = key.HeadComment + "\n" + comment
		}
		key.HeadComment = comment
	}
	return nil
}

// declareVolumes adds the named volumes the extra mounts refer to under the
// top-level volumes, which Compose requires; one the template already
// declares stays as the template declares it.
func declareVolumes(root *yaml.Node, names []string) {
	if len(names) == 0 {
		return
	}
	top := mappingValue(root, "volumes")
	if top == nil || top.Kind != yaml.MappingNode {
		top = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		replaceKey(root, "volumes", top)
	}
	for _, name := range names {
		if mappingValue(top, name) == nil {
			top.Content = append(top.Content, stringNode(name), &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
		}
	}
}

func (m *serviceMerge) mapping(dst, src *yaml.Node, at string) {
	for i := 0; i+1 < len(src.Content); i += 2 {
		key, val := src.Content[i], src.Content[i+1]
		full := key.Value
		if at != "" {
			full = at + "." + key.Value
		}
		cur := mappingValue(dst, key.Value)
		switch {
		case cur == nil:
			dst.Content = append(dst.Content, key, val)
		case at == "" && (key.Value == "environment" || key.Value == "labels"):
			m.keyed(cur, val, full)
		case at == "" && key.Value == "volumes":
			m.volumes(cur, val)
		case cur.Kind == yaml.MappingNode && val.Kind == yaml.MappingNode:
			m.mapping(cur, val, full)
		case cur.Kind == yaml.SequenceNode && val.Kind == yaml.SequenceNode:
			for _, item := range val.Content {
				if !hasItem(cur, item) {
					cur.Content = append(cur.Content, item)
				}
			}
		case cur.Kind == yaml.ScalarNode && val.Kind == yaml.ScalarNode:
			if cur.Value != val.Value {
				m.note("The extra parameters set %s to %s; the template's value %s is replaced.", full, showWords(val.Value), showWords(cur.Value))
				cur.Value, cur.Tag, cur.Style = val.Value, val.Tag, val.Style
			}
		default:
			m.warn(WarnConflict, fmt.Sprintf("The extra parameters set %s in a form the template's own %s cannot be merged with. The template's is kept and the extra parameter is left out.", full, key.Value), full)
		}
	}
}

// keyed merges environment or labels. The template may write either as a map
// or as KEY=VALUE items; an entry for a key the template already sets is kept
// as the template has it, and a different value is a conflict.
func (m *serviceMerge) keyed(cur, val *yaml.Node, full string) {
	for i := 0; i+1 < len(val.Content); i += 2 {
		k, v := val.Content[i], val.Content[i+1]
		var have string
		var present bool
		switch cur.Kind {
		case yaml.MappingNode:
			if existing := mappingValue(cur, k.Value); existing != nil {
				have, present = existing.Value, true
			}
		case yaml.SequenceNode:
			for _, item := range cur.Content {
				if name, value, ok := strings.Cut(item.Value, "="); name == k.Value {
					have, present = value, true
					if !ok {
						have = ""
					}
				}
			}
		default:
			m.warn(WarnConflict, fmt.Sprintf("The extra parameters set %s in a form the template's own %s cannot be merged with. The template's is kept and the extra parameter is left out.", full, full), k.Value)
			continue
		}
		switch {
		case !present && cur.Kind == yaml.MappingNode:
			cur.Content = append(cur.Content, k, v)
		case !present:
			cur.Content = append(cur.Content, stringNode(k.Value+"="+v.Value))
		case have == v.Value:
			m.note("The %s %s repeats the one the template sets and is dropped.", full, k.Value)
		default:
			m.warn(WarnConflict, fmt.Sprintf("The template and the extra parameters both set %s %s, with different values. The template's is kept and the extra parameter is left out. Decide which is right.", full, k.Value), k.Value)
		}
	}
}

// volumes merges mounts by their container path: a second mount of one path
// is not added, because Compose refuses two.
func (m *serviceMerge) volumes(cur, val *yaml.Node) {
	if cur.Kind != yaml.SequenceNode {
		m.warn(WarnConflict, "The extra parameters mount a volume in a form the template's own volumes cannot be merged with. The template's are kept and the extra mounts are left out.", "volumes")
		return
	}
	targets := map[string]*yaml.Node{}
	for _, item := range cur.Content {
		targets[mountTarget(item)] = item
	}
	for _, item := range val.Content {
		target := mountTarget(item)
		existing, taken := targets[target]
		switch {
		case !taken:
			cur.Content = append(cur.Content, item)
			targets[target] = item
		case nodeText(existing) == nodeText(item):
			m.note("The mount at %s repeats the one the template sets and is dropped.", target)
		default:
			m.warn(WarnConflict, fmt.Sprintf("The template and the extra parameters both mount something at %s. The template's is kept and the extra mount is left out. Decide which is right.", target), target)
		}
	}
}

// mountTarget is the container path of a volumes entry, in short or long
// syntax.
func mountTarget(item *yaml.Node) string {
	if item.Kind == yaml.MappingNode {
		if t := mappingValue(item, "target"); t != nil {
			return path.Clean(t.Value)
		}
		return ""
	}
	parts := splitVolume(item.Value)
	if len(parts) >= 2 {
		return path.Clean(parts[1])
	}
	return path.Clean(parts[0])
}

func nodeText(n *yaml.Node) string {
	if n.Kind == yaml.ScalarNode {
		return n.Value
	}
	b, err := yaml.Marshal(n)
	if err != nil {
		return ""
	}
	return string(b)
}

func hasItem(list, item *yaml.Node) bool {
	want := nodeText(item)
	for _, e := range list.Content {
		if nodeText(e) == want {
			return true
		}
	}
	return false
}

func setScalar(m *yaml.Node, key string, value *yaml.Node) {
	replaceKey(m, key, value)
}

func setMapping(m *yaml.Node, key, name string, value *yaml.Node) {
	inner := mappingValue(m, key)
	if inner == nil {
		inner = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		replaceKey(m, key, inner)
	}
	replaceKey(inner, name, value)
}

// replaceKey sets key to value in a mapping node, in place when it is there.
func replaceKey(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, stringNode(key), value)
}

func removeKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
