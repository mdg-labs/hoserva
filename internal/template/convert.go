package template

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mdg-labs/hoserva/internal/container"
)

// ErrInvalidUnraidTemplate is returned when the input is not an Unraid
// container template the converter can read at all.
var ErrInvalidUnraidTemplate = errors.New("not a usable Unraid template")

// MaxUnraidTemplateBytes bounds the template text the converter reads.
const MaxUnraidTemplateBytes = 48 * 1024

// Warning classes (doc 04 §5, Q36). The first five need manual action and
// make a conversion not clean; WarnWritableLayer is its own class and,
// with WarnNote, never counts against a conversion.
const (
	WarnUntranslatedFlag  = "untranslated_flag"
	WarnUntranslatedField = "untranslated_field"
	WarnFlaggedPath       = "flagged_path"
	WarnMissingNetwork    = "missing_network"
	WarnConflict          = "conflict"
	WarnWritableLayer     = "writable_layer"
	WarnNote              = "note"
)

// Warning is one thing the reviewer of a conversion has to know.
type Warning struct {
	Class   string
	Message string
	// Detail is the flag, path, network or entry concerned.
	Detail string
	// Command is the `docker network create` command of a missing network.
	Command string
}

// NetworkDef is what a custom network was created with. The converter
// takes it as input where a caller knows it; the template names only the
// network.
type NetworkDef struct {
	Name    string
	Driver  string
	Subnet  string
	Gateway string
	IPRange string
	// Parent is the host interface of a macvlan or ipvlan network.
	Parent  string
	Options map[string]string
}

// ConvertOptions are the optional inputs of ConvertUnraid.
type ConvertOptions struct {
	Networks []NetworkDef
	// SecretsToEnv keeps the value of every variable the template masks out
	// of the Compose text: the service gets a ${NAME} reference and the value
	// is returned in Conversion.Env, for a caller that stores it in the
	// stack's .env. A masked variable whose name the .env cannot take stays
	// inline, with a note.
	SecretsToEnv bool
}

// UnraidVariable is an environment variable with the description the
// template gave it, kept for an install form.
type UnraidVariable struct {
	Name        string
	Value       string
	Description string
	Secret      bool
}

// UnraidMetadata is the display information of a template that has no
// Compose field.
type UnraidMetadata struct {
	Title      string
	Overview   string
	Category   string
	Support    string
	Project    string
	WebUI      string
	Icon       string
	Requires   string
	DonateLink string
	Variables  []UnraidVariable
}

// Conversion is the result of converting one Unraid template. It is shown
// to the reader and never applied by the converter.
type Conversion struct {
	// Source is the template as it was given.
	Source string
	// Compose is the generated Compose file.
	Compose string
	// Env holds the variables ConvertOptions.SecretsToEnv moved out of the
	// Compose text, by name. It is nil otherwise.
	Env        map[string]string
	Metadata   UnraidMetadata
	Warnings   []Warning
	Privileges []Privilege
}

// EnvFile writes Env as the text of a stack's .env, one line per variable,
// each value written by dotenvValue.
func (c *Conversion) EnvFile() string {
	names := make([]string, 0, len(c.Env))
	for n := range c.Env {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		sb.WriteString(n + "=" + dotenvValue(c.Env[n]) + "\n")
	}
	return sb.String()
}

// Clean reports whether the generated Compose needs no manual action (Q36):
// informational notes and the writable-layer warning are allowed;
// untranslated flags or fields, flagged paths, missing networks and
// conflicts are not.
func (c *Conversion) Clean() bool {
	for _, w := range c.Warnings {
		switch w.Class {
		case WarnWritableLayer, WarnNote:
		default:
			return false
		}
	}
	return true
}

const writableLayerWarning = "The source container may hold configuration this Compose file does not reproduce. A template describes how a container is launched, never what happened inside it afterwards: anything changed with docker exec, hand-edited, or written outside a mapped volume lives only in the container's writable layer. Before recreating the container, check for such state inside the mapped volumes, in docker exec patches and in manual file edits."

type xmlChild struct {
	XMLName xml.Name
}

type xmlElement struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []xmlChild `xml:",any"`
}

func (e xmlElement) childNames() string {
	names := make([]string, len(e.Children))
	for i, ch := range e.Children {
		names[i] = "<" + ch.XMLName.Local + ">"
	}
	return strings.Join(names, " ")
}

func (e xmlElement) attr(name string) string {
	for _, a := range e.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

type xmlContainer struct {
	XMLName  xml.Name
	Elements []xmlElement `xml:",any"`
}

// ignoredElements are Unraid fields that carry no run-time meaning for the
// container: catalog and update bookkeeping.
var ignoredElements = map[string]bool{
	"Registry": true, "TemplateURL": true, "DateInstalled": true, "DonateText": true,
	"Banner": true, "Beta": true, "Branch": true, "ReadMe": true, "Date": true,
	"Changes": true, "TemplateVersion": true, "MinVer": true, "MaxVer": true,
	"Maintainer": true, "Base": true, "ExtraSearchTerms": true,
}

// markupFields are display text that may carry markup; only their text is read.
var markupFields = map[string]bool{"Overview": true, "Description": true}

var (
	serviceNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	invalidNameRun  = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
	imageRefRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)
	cpusetRe        = regexp.MustCompile(`^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$`)
	unraidPortRe    = regexp.MustCompile(`^[0-9]+(-[0-9]+)?$`)
	envNameRe       = regexp.MustCompile(`^[^=\s]+$`)
	knownProtocols  = map[string]bool{"tcp": true, "udp": true, "sctp": true}
	volumeOptionSet = map[string]bool{
		"ro": true, "rw": true, "z": true, "Z": true, "nocopy": true,
		"shared": true, "slave": true, "private": true, "rshared": true, "rslave": true, "rprivate": true,
	}
)

// ConvertUnraid converts an Unraid XML template to a reviewable Compose
// file (doc 04 §5). It reads the template as data, never runs any of it,
// and returns an error only for input that is not a template at all; every
// field it cannot translate is a warning in the result and, for
// ExtraParams, a comment in the Compose file.
func ConvertUnraid(data []byte, opts ConvertOptions) (*Conversion, error) {
	if len(data) > MaxUnraidTemplateBytes {
		return nil, fmt.Errorf("%w: the template is larger than %d bytes", ErrInvalidUnraidTemplate, MaxUnraidTemplateBytes)
	}
	var root xmlContainer
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("%w: not valid XML: %v", ErrInvalidUnraidTemplate, err)
	}
	if root.XMLName.Local != "Container" {
		return nil, fmt.Errorf("%w: the root element is <%s>, not <Container>", ErrInvalidUnraidTemplate, root.XMLName.Local)
	}

	c := newConverter(opts)
	conv := &Conversion{Source: string(data)}
	if err := c.convert(root); err != nil {
		return nil, err
	}
	c.moveSecrets()
	compose, err := c.render()
	if err != nil {
		return nil, err
	}
	conv.Compose = compose
	conv.Env = c.moved
	conv.Metadata = c.meta
	conv.Warnings = append(c.warnings, Warning{Class: WarnWritableLayer, Message: writableLayerWarning})
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(compose), &parsed); err != nil {
		return nil, fmt.Errorf("converting template: the generated Compose does not parse: %w", err)
	}
	conv.Privileges = (&Template{Compose: parsed}).Privileges(nil)
	return conv, nil
}

type merged struct {
	norm   string
	origin string
	desc   string
}

type converter struct {
	opts     ConvertOptions
	svcName  string
	svc      composeService
	meta     UnraidMetadata
	warnings []Warning
	// untranslated are the ExtraParams words shown in the comment.
	untranslated []string

	networks     map[string]composeNetwork
	namedVolumes []string

	hostNet   bool
	volumes   []any
	volIndex  map[string]merged
	ports     []string
	portIndex map[string]merged
	env       map[string]string
	envIndex  map[string]merged
	masked    map[string]bool
	moved     map[string]string
	labels    map[string]string
	labelIdx  map[string]merged
	kv        map[string]map[string]merged
	sysctls   map[string]string
	logOpts   map[string]string
	ulimits   map[string]any
	scalars   map[string]string
	devices   map[string]bool
}

func newConverter(opts ConvertOptions) *converter {
	return &converter{
		opts:      opts,
		volIndex:  map[string]merged{},
		portIndex: map[string]merged{},
		env:       map[string]string{},
		envIndex:  map[string]merged{},
		masked:    map[string]bool{},
		labels:    map[string]string{},
		labelIdx:  map[string]merged{},
		kv:        map[string]map[string]merged{},
		sysctls:   map[string]string{},
		logOpts:   map[string]string{},
		ulimits:   map[string]any{},
		scalars:   map[string]string{},
		devices:   map[string]bool{},
		networks:  map[string]composeNetwork{},
	}
}

func (c *converter) warn(class, message, detail string) {
	c.warnings = append(c.warnings, Warning{Class: class, Message: message, Detail: detail})
}

func (c *converter) note(format string, args ...any) {
	c.warn(WarnNote, fmt.Sprintf(format, args...), "")
}

func (c *converter) convert(root xmlContainer) error {
	seen := map[string]string{}
	var configs []xmlElement
	var network, myIP, repository, name, postArgs, extraParams string
	for _, e := range root.Elements {
		field := e.XMLName.Local
		text := strings.TrimSpace(e.Text)
		if len(e.Children) > 0 && !ignoredElements[field] && !markupFields[field] {
			c.warn(WarnUntranslatedField, fmt.Sprintf("The template field <%s> holds nested elements (%s), which the converter does not read, so none of it is translated. Templates without version=\"2\" describe ports, paths and variables this way; none of those are in the Compose file.", field, e.childNames()), showWords(text))
			continue
		}
		if field == "Config" {
			configs = append(configs, e)
			continue
		}
		if prev, dup := seen[field]; dup {
			if prev != text {
				c.warn(WarnUntranslatedField, fmt.Sprintf("<%s> appears more than once with different values; the first is used and this one is not translated.", field), showWords(text))
			}
			continue
		}
		seen[field] = text
		switch field {
		case "Name":
			name = text
			c.meta.Title = text
		case "Repository":
			repository = text
		case "Network":
			network = text
		case "MyIP":
			myIP = text
		case "Privileged":
			c.privileged(text)
		case "ExtraParams":
			extraParams = text
		case "PostArgs":
			postArgs = text
		case "CPUset":
			c.cpuset(text)
		case "Shell":
			if text != "" {
				c.note("<Shell>%s</Shell> is dropped: it is Unraid's own console setting and has no Compose equivalent.", text)
			}
		case "WebUI":
			c.meta.WebUI = text
		case "Icon":
			c.meta.Icon = text
		case "Overview", "Description":
			if c.meta.Overview == "" {
				c.meta.Overview = text
			}
		case "Category":
			c.meta.Category = text
		case "Support":
			c.meta.Support = text
		case "Project":
			c.meta.Project = text
		case "Requires":
			c.meta.Requires = text
		case "DonateLink":
			c.meta.DonateLink = text
		default:
			if !ignoredElements[field] && text != "" {
				c.warn(WarnUntranslatedField, fmt.Sprintf("The template field <%s> is not one the converter knows, so it is not translated.", field), showWords(text))
			}
		}
	}

	if repository == "" {
		return fmt.Errorf("%w: it has no <Repository>", ErrInvalidUnraidTemplate)
	}
	if !imageRefRe.MatchString(repository) {
		return fmt.Errorf("%w: <Repository> %s is not an image reference", ErrInvalidUnraidTemplate, showWords(repository))
	}
	c.svc.Image = repository
	c.imageTag(repository)
	c.serviceName(name, repository)
	c.network(network, myIP)
	for _, cfg := range configs {
		c.config(cfg)
	}
	c.postArgs(postArgs)
	c.extraParams(extraParams)
	return nil
}

func (c *converter) serviceName(name, repository string) {
	base := strings.TrimSpace(name)
	if base == "" {
		base, _, _ = strings.Cut(path.Base(strings.SplitN(repository, "@", 2)[0]), ":")
		c.note("The template has no <Name>, so the service is named after its image, %s.", base)
	}
	clean := strings.Trim(invalidNameRun.ReplaceAllString(base, "-"), "-._")
	if clean == "" || !serviceNameRe.MatchString(clean) {
		clean = "app"
	}
	if clean != name && name != "" {
		c.note("The name %s is not a valid Compose service name; the service is named %s.", showWords(name), clean)
	}
	c.svcName = clean
	c.svc.ContainerName = clean
}

func (c *converter) imageTag(repository string) {
	if strings.Contains(repository, "@") {
		return
	}
	last := repository[strings.LastIndex(repository, "/")+1:]
	_, tag, hasTag := strings.Cut(last, ":")
	if !hasTag || tag == "latest" {
		c.note("The image %s has no pinned version (it uses the latest tag), so what runs can change from one pull to the next.", repository)
	}
}

func parseTrueFalse(s string) (value, ok bool) {
	switch strings.ToLower(s) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func (c *converter) privileged(text string) {
	if text == "" {
		return
	}
	v, ok := parseTrueFalse(text)
	if !ok {
		c.warn(WarnUntranslatedField, "<Privileged> is neither true nor false, so privileged mode is not set. Check whether the source container ran privileged.", showWords(text))
		return
	}
	c.svc.Privileged = &v
}

func (c *converter) cpuset(text string) {
	if text == "" {
		return
	}
	if !cpusetRe.MatchString(text) {
		c.warn(WarnUntranslatedField, "<CPUset> is not a list of CPU numbers and ranges, so cpuset is not set.", showWords(text))
		return
	}
	c.svc.Cpuset = text
}

func (c *converter) postArgs(text string) {
	if text == "" {
		return
	}
	words, err := shellWords(text)
	if err != nil {
		c.warn(WarnUntranslatedField, "<PostArgs> holds "+err.Error()+", which Unraid's shell acts on and a Compose command cannot express, so it is not translated.", showWords(text))
		return
	}
	c.svc.Command = words
}

// network maps <Network> and <MyIP>: bridge, host, none and container:<name>
// become network_mode, and any other name is a custom network the template
// names and the stack must find already created (Q37).
func (c *converter) network(network, myIP string) {
	custom := ""
	switch {
	case network == "":
	case network == "bridge" || network == "none":
		c.svc.NetworkMode = network
	case network == "host":
		c.svc.NetworkMode = "host"
		c.hostNet = true
	case strings.HasPrefix(network, "container:"):
		if serviceNameRe.MatchString(strings.TrimPrefix(network, "container:")) {
			c.svc.NetworkMode = network
			c.hostNet = true
			c.note("The container shares the network of %s, which has to be running first.", strings.TrimPrefix(network, "container:"))
		} else {
			c.warn(WarnUntranslatedField, "<Network> names a container that is not a valid container name, so the network is not set.", showWords(network))
		}
	case serviceNameRe.MatchString(network):
		custom = network
		c.svc.Networks = map[string]*composeServiceNetwork{network: {}}
		c.networks[network] = composeNetwork{External: true}
		c.missingNetwork(network)
	default:
		c.warn(WarnUntranslatedField, "<Network> is not a valid network name, so the network is not set.", showWords(network))
	}
	if myIP == "" {
		return
	}
	if custom == "" {
		c.warn(WarnUntranslatedField, "<MyIP> sets a fixed address, which only a custom network can give; it is not translated.", showWords(myIP))
		return
	}
	for _, ip := range strings.Split(myIP, ",") {
		addr, err := netip.ParseAddr(strings.TrimSpace(ip))
		if err != nil {
			c.warn(WarnUntranslatedField, "<MyIP> holds an address that is not valid, so it is not translated.", showWords(ip))
			continue
		}
		if addr.Is4() {
			c.svc.Networks[custom].IPv4 = addr.String()
		} else {
			c.svc.Networks[custom].IPv6 = addr.String()
		}
	}
}

func (c *converter) missingNetwork(name string) {
	var def *NetworkDef
	for i := range c.opts.Networks {
		if c.opts.Networks[i].Name == name {
			def = &c.opts.Networks[i]
			break
		}
	}
	cmd, complete := networkCreateCommand(name, def)
	msg := fmt.Sprintf("The template uses the custom network %s, which has to exist before the stack starts. Hoserva does not create networks. If it does not exist yet, create it with the command below.", name)
	if !complete {
		msg += " The command holds <PLACEHOLDER> values because the template does not say how the network was created; fill them in from the network on the source server."
	}
	c.warnings = append(c.warnings, Warning{Class: WarnMissingNetwork, Message: msg, Detail: name, Command: cmd})
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote quotes s for display in a command line a person copies.
func shellQuote(s string) string {
	if s != "" && shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// networkCreateCommand is the command that creates the named network. With a
// definition it holds what the network was created with; a value that is
// not known is a <PLACEHOLDER>, and complete is false.
func networkCreateCommand(name string, def *NetworkDef) (cmd string, complete bool) {
	complete = true
	value := func(v, placeholder string) string {
		if v != "" {
			return shellQuote(v)
		}
		complete = false
		return placeholder
	}
	var d NetworkDef
	if def != nil {
		d = *def
	}
	driver := d.Driver
	if driver == "" {
		driver = "macvlan"
		if def != nil {
			complete = false
		}
	}
	args := []string{"docker", "network", "create", "-d", shellQuote(driver)}
	needsParent := driver == "macvlan" || driver == "ipvlan"
	if d.Subnet != "" || needsParent {
		args = append(args, "--subnet", value(d.Subnet, "<SUBNET>"))
	}
	if d.Gateway != "" {
		args = append(args, "--gateway", shellQuote(d.Gateway))
	}
	if d.IPRange != "" {
		args = append(args, "--ip-range", shellQuote(d.IPRange))
	}
	if d.Parent != "" || needsParent {
		args = append(args, "-o", "parent="+value(d.Parent, "<INTERFACE>"))
	}
	keys := make([]string, 0, len(d.Options))
	for k := range d.Options {
		if k != "parent" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-o", shellQuote(k+"="+d.Options[k]))
	}
	args = append(args, shellQuote(name))
	return strings.Join(args, " "), complete
}

// hostPathProblem says why a host path needs review, or "" when it is in the
// pool or on the cache, where paths map identically (D10).
func hostPathProblem(p string) string {
	clean := path.Clean(p)
	switch {
	case under(clean, "/boot"):
		return "is on Unraid's boot flash drive, which Hoserva does not have at that path"
	case under(clean, "/mnt/disks"):
		return "is an Unassigned Devices mount, which Hoserva does not have at that path"
	case under(clean, "/mnt/user0"):
		return "is Unraid's array-only view of the shares, which has no Hoserva path"
	case insideLayout(clean):
		return ""
	case strings.HasPrefix(clean, "/mnt/") && strings.Count(clean, "/") >= 2 && !under(clean, "/mnt/user") && !under(clean, "/mnt/cache"):
		return "is on another Unraid pool or disk, which Hoserva does not map to this path"
	}
	return "is outside the pool (/mnt/user) and the cache (/mnt/cache)"
}

// checkHostPath reports whether a host path can be used as written. A path
// that is relative is refused here, because Compose would read it as a named
// volume or as relative to the stack directory; one outside the pool and the
// cache is kept as written and flagged for manual review.
func (c *converter) checkHostPath(p, what string) bool {
	if !strings.HasPrefix(p, "/") {
		c.warn(WarnFlaggedPath, fmt.Sprintf("The host path of %s is not absolute, so it is not translated: Compose would read it as something else.", what), showWords(p))
		return false
	}
	if problem := hostPathProblem(p); problem != "" {
		c.warn(WarnFlaggedPath, fmt.Sprintf("The host path of %s %s. It is kept as written; check that it exists and means the same on this server.", what, problem), showWords(p))
	}
	return true
}

func (c *converter) config(e xmlElement) {
	typ := e.attr("Type")
	target := strings.TrimSpace(e.attr("Target"))
	mode := strings.TrimSpace(e.attr("Mode"))
	value := strings.TrimSpace(e.Text)
	label := fmt.Sprintf("the <Config Type=%q Target=%q> entry", typ, target)
	switch typ {
	case "Port":
		c.configPort(label, target, mode, value)
	case "Path":
		c.configPath(label, target, mode, value)
	case "Variable":
		c.meta.Variables = append(c.meta.Variables, UnraidVariable{
			Name: target, Value: value, Description: strings.TrimSpace(e.attr("Description")),
			Secret: strings.EqualFold(e.attr("Mask"), "true"),
		})
		if !envNameRe.MatchString(target) {
			c.warn(WarnUntranslatedField, "A variable entry has no usable name, so it is not translated.", showWords(target, value))
			return
		}
		if value == "" {
			c.note("The variable %s has an empty value and is passed as empty, as the template says.", target)
		}
		c.putEnv(target, value, "the template's Config entry")
		if strings.EqualFold(e.attr("Mask"), "true") && c.env[target] == value {
			c.masked[target] = true
		}
	case "Label":
		if target == "" {
			c.warn(WarnUntranslatedField, "A label entry has no key, so it is not translated.", showWords(value))
			return
		}
		c.putLabel(target, value, "the template's Config entry")
	case "Device":
		if value == "" {
			c.note("A device entry has no device set, so nothing is passed through.")
			return
		}
		if target != "" && target != value {
			c.note("The device entry %s has the container path %s, which Unraid does not use; the device is passed through at its own path.", value, target)
		}
		c.addDevice(value)
	default:
		c.warn(WarnUntranslatedField, fmt.Sprintf("A <Config> entry of type %s is not one the converter knows, so it is not translated.", showWords(typ)), showWords(target, value))
	}
}

func (c *converter) configPort(label, target, mode, value string) {
	if value == "" {
		c.note("%s has no host port set, so the port is not published.", label)
		return
	}
	proto := strings.ToLower(mode)
	if proto == "" {
		proto = "tcp"
	}
	entry := portEntry{host: value, container: target, proto: proto}
	if !entry.valid() || !unraidPortRe.MatchString(value) || strings.Contains(value, "-") != strings.Contains(target, "-") {
		c.warn(WarnUntranslatedField, "A port entry is not a valid host port, container port and protocol, so it is not translated.", showWords(value, target, mode))
		return
	}
	c.addPort(entry, "the template's Config entry")
}

func (c *converter) configPath(label, target, mode, value string) {
	if value == "" {
		c.note("%s has no host path set, so nothing is mounted.", label)
		return
	}
	entry := volEntry{typ: "bind", source: value, target: target}
	opts := []string{}
	if mode != "" {
		opts = strings.Split(mode, ",")
	}
	reason := entry.setOptions(opts)
	switch {
	case reason != "":
	case !path.IsAbs(target):
		reason = "its container path is not absolute"
	case strings.Contains(target, ":") || strings.Contains(value, ":"):
		reason = "a colon in a path would be read by Compose as a separator"
	}
	if reason != "" {
		c.warn(WarnUntranslatedField, "A path entry is not valid ("+reason+"), so it is not translated.", showWords(value, target, mode))
		return
	}
	if !c.checkHostPath(value, fmt.Sprintf("the mount at %s", target)) {
		return
	}
	c.addVolume(entry, "the template's Config entry")
}

// merge decides what a second entry for the same target does (doc 04 §5): an
// identical entry is dropped with a note; one that differs in anything is left
// out, the first one is kept, and the conflict is a review warning. It
// reports whether the caller should add the entry.
func (c *converter) merge(index map[string]merged, kind, key string, m merged) bool {
	prev, exists := index[key]
	if !exists {
		index[key] = m
		return true
	}
	if prev.norm == m.norm {
		c.note("The %s %s from %s repeats the one from %s and is dropped.", kind, key, m.origin, prev.origin)
		return false
	}
	c.warn(WarnConflict, fmt.Sprintf("Two entries for the %s %s differ. %s is kept: %s. %s is left out: %s. Decide which is right.", kind, key, capitalize(prev.origin), prev.desc, capitalize(m.origin), m.desc), key)
	return false
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (c *converter) putEnv(name, value, origin string) {
	if name == "HOST_OS" {
		c.note("The variable HOST_OS is set to %s, which tells the app what system it runs on; check that this is still right.", showWords(value))
	}
	if strings.Contains(value, "$$") {
		c.note("The value of %s contains $$, which is carried as two literal dollar signs; if the app expected a substitution there, review it.", name)
	}
	if c.merge(c.envIndex, "variable", name, merged{norm: value, origin: origin, desc: showWords(name + "=" + value)}) {
		c.env[name] = value
	}
}

var interpolationNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// moveSecrets picks the masked variables that go to the .env when
// ConvertOptions.SecretsToEnv is set. A variable stays inline when it has no
// value to hide, when Compose cannot reference its name, or when its name is
// one the stack layer refuses in a .env because Docker reserves it. Every
// value can be written to the .env (dotenvValue); the one it cannot hold, a
// NUL byte, cannot come out of an XML template.
func (c *converter) moveSecrets() {
	if !c.opts.SecretsToEnv {
		return
	}
	reserved := map[string]bool{}
	for _, n := range container.ReservedEnvNames() {
		reserved[n] = true
	}
	names := make([]string, 0, len(c.masked))
	for n := range c.masked {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := c.env[n]
		switch {
		case v == "":
		case !interpolationNameRe.MatchString(n) || reserved[n]:
			c.note("The value of the masked variable %s stays in the Compose file: its name cannot be used in the stack's environment file. Move the value there by hand if it is a secret.", n)
		default:
			if c.moved == nil {
				c.moved = map[string]string{}
			}
			c.moved[n] = v
		}
	}
}

func (c *converter) putLabel(key, value, origin string) {
	if c.merge(c.labelIdx, "label", key, merged{norm: value, origin: origin, desc: showWords(key + "=" + value)}) {
		c.labels[key] = value
	}
}

func (c *converter) addDevice(dev string) {
	if !strings.Contains(dev, ":") {
		dev += ":" + dev
	}
	if c.devices[dev] {
		c.note("The device %s is listed twice; the repeat is dropped.", dev)
		return
	}
	c.devices[dev] = true
	c.svc.Devices = append(c.svc.Devices, dev)
}

func (c *converter) addPort(p portEntry, origin string) {
	if c.hostNet {
		c.note("The port %s is not published: the container uses the host's network or another container's, where Docker ignores published ports.", p.render())
		return
	}
	if c.merge(c.portIndex, "port", p.container+"/"+p.proto, merged{norm: p.render(), origin: origin, desc: showWords(p.render())}) {
		c.ports = append(c.ports, p.render())
	}
}

func (c *converter) addVolume(v volEntry, origin string) {
	if v.typ == "volume" && v.source != "" {
		known := false
		for _, n := range c.namedVolumes {
			known = known || n == v.source
		}
		if !known {
			c.namedVolumes = append(c.namedVolumes, v.source)
		}
	}
	if c.merge(c.volIndex, "mount", path.Clean(v.target), merged{norm: v.norm(), origin: origin, desc: showWords(v.shortDesc())}) {
		c.volumes = append(c.volumes, v.render())
	}
}

func (c *converter) extraParams(text string) {
	if text == "" {
		return
	}
	words, err := shellWords(text)
	if err != nil {
		c.untranslate(showWords(text), "<ExtraParams> holds "+err.Error()+", which Unraid's shell acts on and cannot be read as flags, so none of it is translated.")
		return
	}
	for _, f := range parseRunFlags(words, runFlagTable) {
		spec, known := runFlagTable[f.Name]
		switch {
		case !known || !f.Known:
			c.untranslate(f.Raw, fmt.Sprintf("The ExtraParams flag %s has no Compose equivalent the converter knows.", f.Raw))
		case f.Missing:
			c.untranslate(f.Raw, fmt.Sprintf("The ExtraParams flag %s needs a value and has none.", f.Raw))
		default:
			if reason := spec.apply(c, f); reason != "" {
				c.untranslate(f.Raw, fmt.Sprintf("The ExtraParams flag %s cannot be translated: %s.", f.Raw, reason))
			}
		}
	}
}

func (c *converter) untranslate(raw, message string) {
	c.untranslated = append(c.untranslated, raw)
	c.warn(WarnUntranslatedFlag, message, raw)
}
