package template

import (
	"bytes"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// quoted is a string that is always written double-quoted: a port mapping
// such as 80:80 reads as a number in YAML 1.1.
type quoted string

func (q quoted) MarshalYAML() (any, error) {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: string(q), Style: yaml.DoubleQuotedStyle}, nil
}

type composeServiceNetwork struct {
	IPv4 string `yaml:"ipv4_address,omitempty"`
	IPv6 string `yaml:"ipv6_address,omitempty"`
}

type composeNetwork struct {
	External bool `yaml:"external"`
}

type composeGPU struct {
	Driver       string     `yaml:"driver"`
	Count        *yaml.Node `yaml:"count,omitempty"`
	DeviceIDs    []string   `yaml:"device_ids,omitempty"`
	Capabilities []string   `yaml:"capabilities"`
}

type composeDeploy struct {
	Resources struct {
		Reservations struct {
			Devices []composeGPU `yaml:"devices"`
		} `yaml:"reservations"`
	} `yaml:"resources"`
}

type composeLogging struct {
	Options map[string]string `yaml:"options"`
}

type composeHealth struct {
	Test        []string `yaml:"test,omitempty"`
	Interval    string   `yaml:"interval,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty"`
	Retries     *int     `yaml:"retries,omitempty"`
	StartPeriod string   `yaml:"start_period,omitempty"`
	Disable     *bool    `yaml:"disable,omitempty"`
}

type composeLongMount struct {
	Type     string `yaml:"type"`
	Source   string `yaml:"source,omitempty"`
	Target   string `yaml:"target"`
	ReadOnly bool   `yaml:"read_only,omitempty"`
	Bind     *struct {
		Propagation string `yaml:"propagation"`
	} `yaml:"bind,omitempty"`
	Volume *struct {
		NoCopy bool `yaml:"nocopy"`
	} `yaml:"volume,omitempty"`
	Tmpfs *struct {
		Size string `yaml:"size"`
	} `yaml:"tmpfs,omitempty"`
}

// composeService is the service the converter writes. Its fields are the
// Compose fields of doc 04 §5, in the order they are written.
type composeService struct {
	Image             string                            `yaml:"image"`
	ContainerName     string                            `yaml:"container_name,omitempty"`
	Restart           string                            `yaml:"restart,omitempty"`
	NetworkMode       string                            `yaml:"network_mode,omitempty"`
	Networks          map[string]*composeServiceNetwork `yaml:"networks,omitempty"`
	Privileged        *bool                             `yaml:"privileged,omitempty"`
	User              string                            `yaml:"user,omitempty"`
	WorkingDir        string                            `yaml:"working_dir,omitempty"`
	Hostname          string                            `yaml:"hostname,omitempty"`
	Cpuset            string                            `yaml:"cpuset,omitempty"`
	Ports             []quoted                          `yaml:"ports,omitempty"`
	Volumes           []any                             `yaml:"volumes,omitempty"`
	Environment       map[string]string                 `yaml:"environment,omitempty"`
	Labels            map[string]string                 `yaml:"labels,omitempty"`
	Devices           []string                          `yaml:"devices,omitempty"`
	Entrypoint        *[]string                         `yaml:"entrypoint,omitempty"`
	Command           []string                          `yaml:"command,omitempty"`
	MemLimit          string                            `yaml:"mem_limit,omitempty"`
	MemswapLimit      *yaml.Node                        `yaml:"memswap_limit,omitempty"`
	Cpus              *yaml.Node                        `yaml:"cpus,omitempty"`
	PidsLimit         *yaml.Node                        `yaml:"pids_limit,omitempty"`
	ShmSize           string                            `yaml:"shm_size,omitempty"`
	GroupAdd          []string                          `yaml:"group_add,omitempty"`
	StdinOpen         *bool                             `yaml:"stdin_open,omitempty"`
	Tty               *bool                             `yaml:"tty,omitempty"`
	Init              *bool                             `yaml:"init,omitempty"`
	ReadOnly          *bool                             `yaml:"read_only,omitempty"`
	CapAdd            []string                          `yaml:"cap_add,omitempty"`
	CapDrop           []string                          `yaml:"cap_drop,omitempty"`
	SecurityOpt       []string                          `yaml:"security_opt,omitempty"`
	Pid               string                            `yaml:"pid,omitempty"`
	Cgroup            string                            `yaml:"cgroup,omitempty"`
	DeviceCgroupRules []string                          `yaml:"device_cgroup_rules,omitempty"`
	Sysctls           map[string]string                 `yaml:"sysctls,omitempty"`
	Ulimits           map[string]any                    `yaml:"ulimits,omitempty"`
	DNS               []string                          `yaml:"dns,omitempty"`
	ExtraHosts        []string                          `yaml:"extra_hosts,omitempty"`
	Tmpfs             []string                          `yaml:"tmpfs,omitempty"`
	Runtime           string                            `yaml:"runtime,omitempty"`
	Deploy            *composeDeploy                    `yaml:"deploy,omitempty"`
	Logging           *composeLogging                   `yaml:"logging,omitempty"`
	StopGracePeriod   string                            `yaml:"stop_grace_period,omitempty"`
	Healthcheck       *composeHealth                    `yaml:"healthcheck,omitempty"`
}

type composeDoc struct {
	Services map[string]composeService `yaml:"services"`
	Networks map[string]composeNetwork `yaml:"networks,omitempty"`
	Volumes  map[string]struct{}       `yaml:"volumes,omitempty"`
}

// render writes the Compose file. Every string value has its $ doubled once,
// here, over the finished document: a template's values are literal text,
// and Compose would otherwise read a $ in them as an interpolation.
func (c *converter) render() (string, error) {
	svc := c.finishedService()
	doc := composeDoc{Services: map[string]composeService{c.svcName: svc}}
	if len(c.networks) > 0 {
		doc.Networks = c.networks
	}
	if len(c.namedVolumes) > 0 {
		doc.Volumes = map[string]struct{}{}
		for _, v := range c.namedVolumes {
			doc.Volumes[v] = struct{}{}
		}
	}

	var n yaml.Node
	if err := n.Encode(doc); err != nil {
		return "", fmt.Errorf("converting template: %w", err)
	}
	escapeDollars(&n)
	if len(c.untranslated) > 0 {
		if key := serviceKeyNode(&n); key != nil {
			lines := []string{"# Hoserva: could not translate the following Unraid ExtraParams:"}
			for _, u := range c.untranslated {
				lines = append(lines, "#   "+u)
			}
			lines = append(lines, "# Review and add the Compose equivalent manually if required.")
			key.HeadComment = strings.Join(lines, "\n")
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return "", fmt.Errorf("converting template: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("converting template: %w", err)
	}
	return buf.String(), nil
}

// finishedService is the service with everything the converter collected
// outside c.svc written into it.
func (c *converter) finishedService() composeService {
	svc := c.svc
	for _, p := range c.ports {
		svc.Ports = append(svc.Ports, quoted(p))
	}
	svc.Volumes = c.volumes
	svc.Environment = nonEmpty(c.env)
	svc.Labels = nonEmpty(c.labels)
	svc.Sysctls = nonEmpty(c.sysctls)
	svc.Ulimits = nonEmptyAny(c.ulimits)
	if len(c.logOpts) > 0 {
		svc.Logging = &composeLogging{Options: c.logOpts}
	}
	return svc
}

// fragment is the service's own settings as a mapping node, without the
// image, for a caller that merges them into a service it already has. It
// returns only the service mapping: the named volumes its mounts refer to are
// in c.namedVolumes, which the caller declares at the top level as render
// does. Every string has its $ doubled, as render does.
func (c *converter) fragment() (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(c.finishedService()); err != nil {
		return nil, fmt.Errorf("reading the extra parameters: %w", err)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "image" {
			n.Content = append(n.Content[:i], n.Content[i+2:]...)
			break
		}
	}
	escapeDollars(&n)
	return &n, nil
}

func nonEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

func nonEmptyAny(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	return m
}

func serviceKeyNode(root *yaml.Node) *yaml.Node {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "services" && len(root.Content[i+1].Content) > 0 {
			return root.Content[i+1].Content[0]
		}
	}
	return nil
}

func escapeDollars(n *yaml.Node) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			escapeDollars(n.Content[i])
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			escapeDollars(item)
		}
	case yaml.ScalarNode:
		if n.Tag == "!!str" {
			n.Value = strings.ReplaceAll(n.Value, "$", "$$")
		}
	}
}

func numberNode(v string) *yaml.Node {
	tag := "!!int"
	if strings.Contains(v, ".") {
		tag = "!!float"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v}
}

func stringNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// portEntry is one published port: [ip:][host:]container/protocol, where
// host and container may be ranges.
type portEntry struct {
	ip, host, container, proto string
}

func validPortNumbers(s string) bool {
	if !unraidPortRe.MatchString(s) {
		return false
	}
	lo, hi, isRange := strings.Cut(s, "-")
	l, err := strconv.Atoi(lo)
	if err != nil || l < 1 || l > 65535 {
		return false
	}
	if !isRange {
		return true
	}
	h, err := strconv.Atoi(hi)
	return err == nil && h >= l && h <= 65535
}

func (p portEntry) valid() bool {
	if !validPortNumbers(p.container) || !knownProtocols[p.proto] {
		return false
	}
	if p.host != "" && !validPortNumbers(p.host) {
		return false
	}
	if p.ip != "" {
		if _, err := netip.ParseAddr(p.ip); err != nil {
			return false
		}
	}
	return true
}

func (p portEntry) render() string {
	var sb strings.Builder
	if p.ip != "" {
		if strings.Contains(p.ip, ":") {
			sb.WriteString("[" + p.ip + "]:")
		} else {
			sb.WriteString(p.ip + ":")
		}
		sb.WriteString(p.host + ":")
	} else if p.host != "" {
		sb.WriteString(p.host + ":")
	}
	sb.WriteString(p.container + "/" + p.proto)
	return sb.String()
}

// parsePublish reads the value of docker run's --publish:
// [ip:][hostPort:]containerPort[/protocol].
func parsePublish(s string) (portEntry, bool) {
	spec, proto, hasProto := strings.Cut(s, "/")
	if !hasProto {
		proto = "tcp"
	}
	p := portEntry{proto: strings.ToLower(proto)}
	if strings.HasPrefix(spec, "[") {
		end := strings.Index(spec, "]:")
		if end < 0 {
			return portEntry{}, false
		}
		p.ip = spec[1:end]
		parts := strings.Split(spec[end+2:], ":")
		if len(parts) != 2 {
			return portEntry{}, false
		}
		p.host, p.container = parts[0], parts[1]
		return p, p.valid()
	}
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 1:
		p.container = parts[0]
	case 2:
		p.host, p.container = parts[0], parts[1]
	case 3:
		p.ip, p.host, p.container = parts[0], parts[1], parts[2]
	default:
		return portEntry{}, false
	}
	return p, p.valid()
}

// volEntry is one mount, from a Config path entry, --volume or --mount.
type volEntry struct {
	// long is set on a --mount entry, which is written in Compose's long
	// syntax.
	long      bool
	typ       string
	source    string
	target    string
	ro        bool
	opts      []string
	tmpfsSize string
}

// setOptions reads short-syntax mount options and returns why they cannot be
// translated, or "".
func (v *volEntry) setOptions(opts []string) string {
	seen := map[string]bool{}
	for _, o := range opts {
		o = strings.TrimSpace(o)
		if !volumeOptionSet[o] {
			return fmt.Sprintf("the mount option %s is not one the converter knows", showWords(o))
		}
		seen[o] = true
	}
	if seen["ro"] && seen["rw"] {
		return "the mount is both ro and rw"
	}
	v.ro = v.ro || seen["ro"]
	v.opts = nil
	for o := range seen {
		if o != "ro" && o != "rw" {
			v.opts = append(v.opts, o)
		}
	}
	sort.Strings(v.opts)
	return ""
}

func (v volEntry) norm() string {
	return strings.Join([]string{v.typ, v.source, path.Clean(v.target), strconv.FormatBool(v.ro), strings.Join(v.opts, ","), v.tmpfsSize}, "|")
}

func (v volEntry) shortDesc() string {
	var parts []string
	if v.source != "" {
		parts = append(parts, v.source)
	}
	parts = append(parts, v.target)
	var opts []string
	if v.ro {
		opts = append(opts, "ro")
	}
	opts = append(opts, v.opts...)
	if len(opts) > 0 && v.source != "" {
		parts = append(parts, strings.Join(opts, ","))
	}
	return strings.Join(parts, ":")
}

var propagationModes = map[string]bool{"shared": true, "slave": true, "private": true, "rshared": true, "rslave": true, "rprivate": true}

func (v volEntry) render() any {
	if !v.long {
		return v.shortDesc()
	}
	m := composeLongMount{Type: v.typ, Source: v.source, Target: v.target, ReadOnly: v.ro}
	for _, o := range v.opts {
		switch {
		case propagationModes[o]:
			m.Bind = &struct {
				Propagation string `yaml:"propagation"`
			}{o}
		case o == "nocopy":
			m.Volume = &struct {
				NoCopy bool `yaml:"nocopy"`
			}{true}
		}
	}
	if v.tmpfsSize != "" {
		m.Tmpfs = &struct {
			Size string `yaml:"size"`
		}{v.tmpfsSize}
	}
	return m
}

var volumeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
