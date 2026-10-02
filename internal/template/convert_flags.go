package template

import (
	"encoding/csv"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// runFlagSpec is one row of doc 04 §5's translate table, keyed by long form:
// whether the flag takes a value, and how it becomes Compose. apply returns
// why the flag cannot be translated, or "" once it has been.
type runFlagSpec struct {
	takesValue bool
	apply      func(c *converter, f runFlag) string
}

var (
	memoryRe   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[kKmMgGtTpP]?[iI]?[bB]?$`)
	cpusRe     = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	intRe      = regexp.MustCompile(`^-?[0-9]+$`)
	uintRe     = regexp.MustCompile(`^[0-9]+$`)
	restartRe  = regexp.MustCompile(`^(no|always|unless-stopped|on-failure(:[0-9]+)?)$`)
	capRe      = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	runtimeRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	ulimitRe   = regexp.MustCompile(`^[a-z]+$`)
	cgroupRule = regexp.MustCompile(`^[abc] ([0-9]+|\*):([0-9]+|\*) [rwm]{1,3}$`)
	devicePerm = regexp.MustCompile(`^[rwm]{1,3}$`)
)

var runFlagTable map[string]runFlagSpec

func init() {
	runFlagTable = map[string]runFlagSpec{
		"--restart":             {true, matching("restart", restartRe, func(c *converter, v string) { c.svc.Restart = v })},
		"--memory":              {true, matching("memory limit", memoryRe, func(c *converter, v string) { c.svc.MemLimit = v })},
		"--memory-swap":         {true, memorySwap},
		"--cpus":                {true, matching("number of CPUs", cpusRe, func(c *converter, v string) { c.svc.Cpus = numberNode(v) })},
		"--pids-limit":          {true, matching("process limit", intRe, func(c *converter, v string) { c.svc.PidsLimit = numberNode(v) })},
		"--user":                {true, nonBlank("user", func(c *converter, v string) { c.svc.User = v })},
		"--workdir":             {true, nonBlank("working directory", func(c *converter, v string) { c.svc.WorkingDir = v })},
		"--hostname":            {true, nonBlank("hostname", func(c *converter, v string) { c.svc.Hostname = v })},
		"--group-add":           {true, nonBlankEach("group", func(c *converter, v string) { c.svc.GroupAdd = appendOnce(c.svc.GroupAdd, v) })},
		"--entrypoint":          {true, entrypoint},
		"--interactive":         {false, boolean(func(c *converter, v bool) { c.svc.StdinOpen = &v })},
		"--tty":                 {false, boolean(func(c *converter, v bool) { c.svc.Tty = &v })},
		"--init":                {false, boolean(func(c *converter, v bool) { c.svc.Init = &v })},
		"--read-only":           {false, boolean(func(c *converter, v bool) { c.svc.ReadOnly = &v })},
		"--device":              {true, device},
		"--cap-add":             {true, matchingEach("capability", capRe, func(c *converter, v string) { c.svc.CapAdd = appendOnce(c.svc.CapAdd, v) })},
		"--cap-drop":            {true, matchingEach("capability", capRe, func(c *converter, v string) { c.svc.CapDrop = appendOnce(c.svc.CapDrop, v) })},
		"--security-opt":        {true, nonBlankEach("security option", func(c *converter, v string) { c.svc.SecurityOpt = appendOnce(c.svc.SecurityOpt, v) })},
		"--sysctl":              {true, keyValue("sysctl", func(c *converter) map[string]string { return c.sysctls })},
		"--ulimit":              {true, ulimit},
		"--dns":                 {true, dns},
		"--add-host":            {true, addHost},
		"--tmpfs":               {true, tmpfs},
		"--shm-size":            {true, matching("size", memoryRe, func(c *converter, v string) { c.svc.ShmSize = v })},
		"--runtime":             {true, matching("runtime", runtimeRe, func(c *converter, v string) { c.svc.Runtime = v })},
		"--gpus":                {true, gpus},
		"--log-opt":             {true, keyValue("log option", func(c *converter) map[string]string { return c.logOpts })},
		"--stop-timeout":        {true, matching("number of seconds", uintRe, func(c *converter, v string) { c.svc.StopGracePeriod = v + "s" })},
		"--health-cmd":          {true, nonBlank("health check command", func(c *converter, v string) { c.health().Test = []string{"CMD-SHELL", v} })},
		"--health-interval":     {true, duration("interval", func(h *composeHealth, v string) { h.Interval = v })},
		"--health-timeout":      {true, duration("timeout", func(h *composeHealth, v string) { h.Timeout = v })},
		"--health-start-period": {true, duration("start period", func(h *composeHealth, v string) { h.StartPeriod = v })},
		"--health-retries": {true, matching("number of retries", uintRe, func(c *converter, v string) {
			n, _ := strconv.Atoi(v)
			c.health().Retries = &n
		})},
		"--no-healthcheck": {false, boolean(func(c *converter, v bool) {
			if v {
				c.health().Disable = &v
			}
		})},
		"--volume":             {true, volumeFlag},
		"--mount":              {true, mountFlag},
		"--publish":            {true, publishFlag},
		"--env":                {true, envFlag},
		"--pid":                {true, pid},
		"--cgroupns":           {true, cgroupns},
		"--device-cgroup-rule": {true, matchingEach("device cgroup rule", cgroupRule, func(c *converter, v string) { c.svc.DeviceCgroupRules = appendOnce(c.svc.DeviceCgroupRules, v) })},
	}
}

func appendOnce(list []string, v string) []string {
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}

func (c *converter) health() *composeHealth {
	if c.svc.Healthcheck == nil {
		c.svc.Healthcheck = &composeHealth{}
	}
	return c.svc.Healthcheck
}

// scalar records a single-valued flag. Given again with another value, the
// last one is used, as docker run does, and the reviewer is told.
func (c *converter) scalar(f runFlag) {
	if prev, ok := c.scalars[f.Name]; ok && prev != f.Value {
		c.note("The ExtraParams flag %s is given more than once; the last value, %s, is used, as docker run does.", f.Name, showWords(f.Value))
	}
	c.scalars[f.Name] = f.Value
}

// single makes a repeatable flag's apply single-valued: once a value is
// accepted, scalar notes a repeat that overrides an earlier one.
func single(apply func(*converter, runFlag) string) func(*converter, runFlag) string {
	return func(c *converter, f runFlag) string {
		if why := apply(c, f); why != "" {
			return why
		}
		c.scalar(f)
		return ""
	}
}

func matching(what string, re *regexp.Regexp, set func(c *converter, v string)) func(*converter, runFlag) string {
	return single(matchingEach(what, re, set))
}

// matchingEach is matching for a flag docker run repeats into a list, where
// every value is kept.
func matchingEach(what string, re *regexp.Regexp, set func(c *converter, v string)) func(*converter, runFlag) string {
	return func(c *converter, f runFlag) string {
		if !re.MatchString(f.Value) {
			return fmt.Sprintf("%s is not a valid %s", showWords(f.Value), what)
		}
		set(c, f.Value)
		return ""
	}
}

func nonBlank(what string, set func(c *converter, v string)) func(*converter, runFlag) string {
	return single(nonBlankEach(what, set))
}

// nonBlankEach is nonBlank for a flag docker run repeats into a list.
func nonBlankEach(what string, set func(c *converter, v string)) func(*converter, runFlag) string {
	return func(c *converter, f runFlag) string {
		if strings.TrimSpace(f.Value) == "" {
			return "the " + what + " is empty"
		}
		set(c, f.Value)
		return ""
	}
}

func boolean(set func(c *converter, v bool)) func(*converter, runFlag) string {
	return func(c *converter, f runFlag) string {
		v := true
		if f.HasValue {
			var err error
			if v, err = strconv.ParseBool(f.Value); err != nil {
				return fmt.Sprintf("%s is not true or false", showWords(f.Value))
			}
		}
		c.scalar(runFlag{Name: f.Name, Value: strconv.FormatBool(v)})
		set(c, v)
		return ""
	}
}

func memorySwap(c *converter, f runFlag) string {
	switch {
	case f.Value == "-1":
		c.svc.MemswapLimit = numberNode("-1")
	case memoryRe.MatchString(f.Value):
		c.svc.MemswapLimit = stringNode(f.Value)
	default:
		return fmt.Sprintf("%s is not a valid memory limit", showWords(f.Value))
	}
	c.scalar(f)
	return ""
}

// entrypoint carries the value as one executable, never split on spaces
// (doc 04 §5): an empty value clears the image's entrypoint.
func entrypoint(c *converter, f runFlag) string {
	c.scalar(f)
	list := []string{f.Value}
	if f.Value == "" {
		list = []string{}
	}
	c.svc.Entrypoint = &list
	return ""
}

func device(c *converter, f runFlag) string {
	parts := strings.Split(f.Value, ":")
	if len(parts) > 3 || !strings.HasPrefix(parts[0], "/") || (len(parts) == 3 && !devicePerm.MatchString(parts[2])) || (len(parts) >= 2 && !strings.HasPrefix(parts[1], "/")) {
		return fmt.Sprintf("%s is not HOST[:CONTAINER[:PERMISSIONS]] with absolute paths", showWords(f.Value))
	}
	c.addDevice(f.Value)
	return ""
}

func keyValue(what string, target func(c *converter) map[string]string) func(*converter, runFlag) string {
	return func(c *converter, f runFlag) string {
		k, v, ok := strings.Cut(f.Value, "=")
		if !ok || k == "" {
			return fmt.Sprintf("%s is not KEY=VALUE", showWords(f.Value))
		}
		idx := c.kv[what]
		if idx == nil {
			idx = map[string]merged{}
			c.kv[what] = idx
		}
		if c.merge(idx, what, k, merged{norm: v, origin: "ExtraParams", desc: showWords(f.Value)}) {
			target(c)[k] = v
		}
		return ""
	}
}

func ulimit(c *converter, f runFlag) string {
	name, limits, ok := strings.Cut(f.Value, "=")
	soft, hard, hasHard := strings.Cut(limits, ":")
	if !ok || !ulimitRe.MatchString(name) || !intRe.MatchString(soft) || (hasHard && !intRe.MatchString(hard)) {
		return fmt.Sprintf("%s is not NAME=SOFT[:HARD]", showWords(f.Value))
	}
	idx := c.kv["ulimit"]
	if idx == nil {
		idx = map[string]merged{}
		c.kv["ulimit"] = idx
	}
	if !c.merge(idx, "ulimit", name, merged{norm: limits, origin: "ExtraParams", desc: showWords(f.Value)}) {
		return ""
	}
	if hasHard {
		c.ulimits[name] = map[string]any{"soft": numberNode(soft), "hard": numberNode(hard)}
	} else {
		c.ulimits[name] = numberNode(soft)
	}
	return ""
}

func dns(c *converter, f runFlag) string {
	if _, err := netip.ParseAddr(f.Value); err != nil {
		return fmt.Sprintf("%s is not an IP address", showWords(f.Value))
	}
	c.svc.DNS = appendOnce(c.svc.DNS, f.Value)
	return ""
}

func addHost(c *converter, f runFlag) string {
	host, ip, ok := strings.Cut(f.Value, ":")
	if !ok {
		host, ip, ok = strings.Cut(f.Value, "=")
	}
	if _, err := netip.ParseAddr(ip); !ok || host == "" || (err != nil && ip != "host-gateway") {
		return fmt.Sprintf("%s is not HOST:IP", showWords(f.Value))
	}
	c.svc.ExtraHosts = appendOnce(c.svc.ExtraHosts, host+":"+ip)
	return ""
}

func tmpfs(c *converter, f runFlag) string {
	dir, _, _ := strings.Cut(f.Value, ":")
	if !path.IsAbs(dir) {
		return fmt.Sprintf("%s does not start with an absolute path", showWords(f.Value))
	}
	c.svc.Tmpfs = appendOnce(c.svc.Tmpfs, f.Value)
	return ""
}

func duration(what string, set func(h *composeHealth, v string)) func(*converter, runFlag) string {
	return func(c *converter, f runFlag) string {
		d, err := time.ParseDuration(f.Value)
		if err != nil || d < 0 {
			return fmt.Sprintf("%s is not a valid %s", showWords(f.Value), what)
		}
		c.scalar(f)
		set(c.health(), f.Value)
		return ""
	}
}

func pid(c *converter, f runFlag) string {
	shared := strings.HasPrefix(f.Value, "container:") && serviceNameRe.MatchString(strings.TrimPrefix(f.Value, "container:"))
	if f.Value != "host" && !shared {
		return fmt.Sprintf("%s is not host or container:NAME", showWords(f.Value))
	}
	c.scalar(f)
	c.svc.Pid = f.Value
	return ""
}

func cgroupns(c *converter, f runFlag) string {
	if f.Value != "host" && f.Value != "private" {
		return fmt.Sprintf("%s is not host or private", showWords(f.Value))
	}
	c.scalar(f)
	c.svc.Cgroup = f.Value
	return ""
}

// gpus reads --gpus: all, a count, or comma-separated fields (driver, count,
// device, capabilities).
func gpus(c *converter, f runFlag) string {
	g := composeGPU{Driver: "nvidia", Capabilities: []string{"gpu"}}
	switch {
	case f.Value == "all":
		g.Count = stringNode("all")
	case uintRe.MatchString(f.Value):
		g.Count = numberNode(f.Value)
	default:
		r := csv.NewReader(strings.NewReader(f.Value))
		r.FieldsPerRecord = -1
		fields, err := r.Read()
		if err != nil {
			return fmt.Sprintf("%s is not a list of fields", showWords(f.Value))
		}
		seen := map[string]bool{}
		for _, field := range fields {
			k, v, ok := strings.Cut(field, "=")
			if !ok || v == "" || seen[k] {
				return fmt.Sprintf("the field %s is not a single KEY=VALUE", showWords(field))
			}
			seen[k] = true
			switch k {
			case "driver":
				if !runtimeRe.MatchString(v) {
					return fmt.Sprintf("%s is not a valid driver", showWords(v))
				}
				g.Driver = v
			case "count":
				switch {
				case v == "all" || v == "-1":
					g.Count = stringNode("all")
				case uintRe.MatchString(v):
					g.Count = numberNode(v)
				default:
					return fmt.Sprintf("%s is not a valid count", showWords(v))
				}
			case "device":
				g.DeviceIDs = strings.Split(v, ",")
			case "capabilities":
				g.Capabilities = strings.Split(v, ",")
				for _, cp := range g.Capabilities {
					if !runtimeRe.MatchString(cp) {
						return fmt.Sprintf("%s is not a valid capability", showWords(cp))
					}
				}
			default:
				return fmt.Sprintf("the field %s is not one the converter knows", showWords(k))
			}
		}
		if seen["count"] && seen["device"] {
			return "count and device cannot be combined"
		}
		if g.Count == nil && len(g.DeviceIDs) == 0 {
			g.Count = stringNode("all")
		}
	}
	if c.svc.Deploy == nil {
		c.svc.Deploy = &composeDeploy{}
	}
	d := &c.svc.Deploy.Resources.Reservations
	d.Devices = append(d.Devices, g)
	return ""
}

func volumeFlag(c *converter, f runFlag) string {
	parts := strings.Split(f.Value, ":")
	var v volEntry
	var opts []string
	switch len(parts) {
	case 1:
		v = volEntry{typ: "volume", target: parts[0]}
	case 2:
		v = volEntry{source: parts[0], target: parts[1]}
	case 3:
		v = volEntry{source: parts[0], target: parts[1]}
		opts = strings.Split(parts[2], ",")
	default:
		return fmt.Sprintf("%s is not SOURCE:TARGET[:OPTIONS]", showWords(f.Value))
	}
	if !path.IsAbs(v.target) {
		return fmt.Sprintf("the container path %s is not absolute", showWords(v.target))
	}
	if len(parts) > 1 {
		switch {
		case strings.HasPrefix(v.source, "/"):
			v.typ = "bind"
		case volumeNameRe.MatchString(v.source):
			v.typ = "volume"
		default:
			return fmt.Sprintf("the source %s is neither an absolute path nor a volume name", showWords(v.source))
		}
	}
	if reason := v.setOptions(opts); reason != "" {
		return reason
	}
	if v.typ == "bind" {
		c.checkHostPath(v.source, fmt.Sprintf("the mount at %s", v.target))
	}
	c.addVolume(v, "the ExtraParams flag "+f.Raw)
	return ""
}

func mountFlag(c *converter, f runFlag) string {
	r := csv.NewReader(strings.NewReader(f.Value))
	r.FieldsPerRecord = -1
	fields, err := r.Read()
	if err != nil {
		return fmt.Sprintf("%s is not a list of key=value fields", showWords(f.Value))
	}
	v := volEntry{long: true, typ: "volume"}
	seen := map[string]bool{}
	var opts []string
	for _, field := range fields {
		k, val, hasVal := strings.Cut(field, "=")
		if seen[k] {
			return fmt.Sprintf("the field %s is given twice", showWords(k))
		}
		seen[k] = true
		switch k {
		case "type":
			if val != "bind" && val != "volume" && val != "tmpfs" {
				return fmt.Sprintf("the mount type %s is not bind, volume or tmpfs", showWords(val))
			}
			v.typ = val
		case "source", "src":
			v.source = val
		case "target", "destination", "dst":
			v.target = val
		case "readonly", "ro":
			on := true
			if hasVal {
				if on, err = strconv.ParseBool(val); err != nil {
					return fmt.Sprintf("%s is not true or false", showWords(val))
				}
			}
			v.ro = on
		case "bind-propagation":
			if !propagationModes[val] {
				return fmt.Sprintf("%s is not a bind propagation mode", showWords(val))
			}
			opts = append(opts, val)
		case "volume-nocopy":
			on := true
			if hasVal {
				if on, err = strconv.ParseBool(val); err != nil {
					return fmt.Sprintf("%s is not true or false", showWords(val))
				}
			}
			if on {
				opts = append(opts, "nocopy")
			}
		case "tmpfs-size":
			if !memoryRe.MatchString(val) {
				return fmt.Sprintf("%s is not a valid size", showWords(val))
			}
			v.tmpfsSize = val
		default:
			return fmt.Sprintf("the mount field %s is not one the converter knows", showWords(k))
		}
	}
	if !path.IsAbs(v.target) {
		return fmt.Sprintf("the container path %s is not absolute", showWords(v.target))
	}
	switch v.typ {
	case "bind":
		if !strings.HasPrefix(v.source, "/") {
			return fmt.Sprintf("a bind mount needs an absolute source, not %s", showWords(v.source))
		}
	case "volume":
		if v.source != "" && !volumeNameRe.MatchString(v.source) {
			return fmt.Sprintf("%s is not a volume name", showWords(v.source))
		}
	case "tmpfs":
		if v.source != "" {
			return "a tmpfs mount has no source"
		}
	}
	if (seen["bind-propagation"] && v.typ != "bind") || (seen["volume-nocopy"] && v.typ != "volume") || (seen["tmpfs-size"] && v.typ != "tmpfs") {
		return "an option does not belong to this mount type"
	}
	if reason := v.setOptions(opts); reason != "" {
		return reason
	}
	if v.typ == "bind" {
		c.checkHostPath(v.source, fmt.Sprintf("the mount at %s", v.target))
	}
	c.addVolume(v, "the ExtraParams flag "+f.Raw)
	return ""
}

func publishFlag(c *converter, f runFlag) string {
	p, ok := parsePublish(f.Value)
	if !ok {
		return fmt.Sprintf("%s is not [IP:][HOSTPORT:]CONTAINERPORT[/PROTOCOL]", showWords(f.Value))
	}
	c.addPort(p, "the ExtraParams flag "+f.Raw)
	return ""
}

func envFlag(c *converter, f runFlag) string {
	k, v, ok := strings.Cut(f.Value, "=")
	if !envNameRe.MatchString(k) {
		return fmt.Sprintf("%s has no variable name", showWords(f.Value))
	}
	if !ok {
		return "a variable with no value inherits it from the host's environment, which a Compose file cannot express"
	}
	c.putEnv(k, v, "the ExtraParams flag "+f.Raw)
	return ""
}
