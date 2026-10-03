package migrate

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/pool"
)

// DiskDirs lists the names of the top-level directories on a disk's filesystem:
// the shares that have data on it. It is how the scan tells a share from the
// config of one that no longer exists. A nil DiskDirs on the Scanner means the
// scan does not read the disks' contents, and no share config is then called an
// orphan.
type DiskDirs interface {
	TopLevelDirs(ctx context.Context, d disk.Disk) ([]string, error)
}

// dirIndex records which matched disks hold a top-level directory per share.
type dirIndex struct {
	read bool
	// complete is true when every data disk and pool device the configuration
	// names was matched and listed: only then does a directory found nowhere
	// mean there is none.
	complete bool
	problems []string
	data     map[string][]string
	pools    map[string][]string
}

func addUnique(m map[string][]string, key, v string) {
	for _, x := range m[key] {
		if x == v {
			return
		}
	}
	m[key] = append(m[key], v)
}

func buildDirIndex(ctx context.Context, dirs DiskDirs, members []*member) (*dirIndex, error) {
	d := &dirIndex{data: map[string][]string{}, pools: map[string][]string{}}
	if dirs == nil {
		return d, nil
	}
	d.read = true
	considered, listed := 0, 0
	for _, m := range members {
		if m.role != RoleData && m.role != RoleCache {
			continue
		}
		considered++
		if m.disk == nil {
			d.problems = append(d.problems, fmt.Sprintf("%s: %s", m.subject, m.problem))
			continue
		}
		names, err := dirs.TopLevelDirs(ctx, *m.disk)
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if err != nil {
			d.problems = append(d.problems, fmt.Sprintf("%s: %v", m.subject, err))
			continue
		}
		listed++
		target := d.data
		if m.role == RoleCache {
			target = d.pools
		}
		for _, n := range names {
			if !strings.HasPrefix(n, ".") {
				addUnique(target, n, m.subject)
			}
		}
	}
	d.complete = considered > 0 && listed == considered
	return d, nil
}

func joinNames(names []string) string {
	return strings.Join(names, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// checkShares reports every share config, and keeps the ones with a share behind them in the import.
func checkShares(r *Report, src FlashSource, imp *Import, dirs *dirIndex) error {
	shareCfg, err := readCfg(r, CheckShares, src, "config/share.cfg")
	if err != nil {
		return err
	}
	globals := shareCfg.flat()
	var iniShares map[string]bool
	if data, found, err := readOptional(src, captureDir+"/shares.ini"); err != nil {
		return err
	} else if found {
		if ini, err := parseCfg(data); err != nil {
			r.add(CheckShares, StatusWarn, "", "The capture's shares.ini could not be read (%v), so it is not used to tell a deleted share from a real one.", err)
		} else {
			iniShares = map[string]bool{}
			for _, s := range ini.sections {
				if s != "" {
					iniShares[s] = true
				}
			}
		}
	}

	var files []string
	for _, n := range directChildren(src, sharesDir) {
		if strings.HasSuffix(n, ".cfg") {
			files = append(files, n)
		}
	}
	if len(files) == 0 {
		r.add(CheckShares, StatusWarn, "", "No share configuration was found in config/shares/, so no share can be seeded. If the server had shares, this source is incomplete.")
		return nil
	}

	var shares []Share
	var orphans []string
	unreadable := 0
	for _, n := range files {
		name := strings.TrimSuffix(path.Base(n), ".cfg")
		data, err := src.Read(n)
		if err != nil {
			return err
		}
		sh, err := parseShare(name, data, globals)
		if err != nil {
			unreadable++
			r.add(CheckShares, StatusWarn, name, "The share config could not be read (%v). The share is not carried over.", err)
			continue
		}
		if dirs.complete && len(dirs.data[name]) == 0 && len(dirs.pools[name]) == 0 && !iniShares[name] {
			orphans = append(orphans, name)
			continue
		}
		shares = append(shares, sh)
	}
	sort.Slice(shares, func(i, j int) bool { return shares[i].Name < shares[j].Name })
	sort.Strings(orphans)
	imp.Shares = shares

	summary := fmt.Sprintf("%d %s configured", len(shares), plural(len(shares), "share", "shares"))
	if len(orphans) > 0 {
		summary += fmt.Sprintf("; %d %s with no share behind %s", len(orphans), plural(len(orphans), "config", "configs"), plural(len(orphans), "it", "them"))
	}
	r.add(CheckShares, StatusInfo, "", "%s. Each share's allocation method and cache setting are mapped below (Q11).", summary)
	switch {
	case !dirs.read:
		r.add(CheckShares, StatusInfo, "", "This scan does not read the disks' directories, so the config of a share that no longer exists cannot be told from a real one: every share config is kept.")
	case !dirs.complete:
		r.add(CheckShares, StatusWarn, "", "A disk's directories could not be read (%s). A disk that is missing or unreadable can hold a share's only directory, so no config is called an orphan and every one is kept.", strings.Join(dirs.problems, "; "))
	}
	for _, o := range orphans {
		r.add(CheckShares, StatusInfo, o, "The config has no share behind it: no top-level directory of this name is on any matched disk. It is an orphan, and is not carried over.")
	}
	for _, sh := range shares {
		addShareRow(r, sh, dirs)
	}
	return nil
}

func addShareRow(r *Report, sh Share, dirs *dirIndex) {
	st := StatusInfo
	worse := func(s Status) {
		if s == StatusFlag || (s == StatusWarn && st == StatusInfo) {
			st = s
		}
	}
	var parts []string

	policy, exact, known := allocationPolicy(sh.Allocator)
	switch {
	case !known:
		worse(StatusWarn)
		parts = append(parts, fmt.Sprintf("allocation method %s is not one Hoserva maps, so the share gets the default policy (%s) unless you choose another", allocationLabel(sh.Allocator), pool.DefaultCreatePolicy.Label()))
	case exact:
		parts = append(parts, fmt.Sprintf("allocation %s maps to %s (%s)", allocationLabel(sh.Allocator), policy.Label(), policy))
	default:
		worse(StatusFlag)
		parts = append(parts, fmt.Sprintf("allocation %s: no exact equivalent, mapped to %s (%s) (Q11)", allocationLabel(sh.Allocator), policy.Label(), policy))
	}

	if mode, ok := cacheModeFor(sh.UseCache); !ok {
		worse(StatusWarn)
		parts = append(parts, fmt.Sprintf("cache setting %q is not one Hoserva maps", sh.UseCache))
	} else if sh.UseCache == "prefer" {
		parts = append(parts, fmt.Sprintf("cache setting prefer maps to %s (Unraid's prefer also lets writes fall through to the array, which cache-only does not)", mode))
	} else {
		parts = append(parts, fmt.Sprintf("cache setting %s maps to %s", sh.UseCache, mode))
	}

	if sh.Export == "" || sh.Export == "-" {
		parts = append(parts, "not exported over SMB")
	} else {
		parts = append(parts, fmt.Sprintf("exported over SMB (%s)", sh.Export))
	}
	if sh.SplitLevel != "" {
		parts = append(parts, "split level "+sh.SplitLevel)
	}
	if sh.Floor != "" && sh.Floor != "0" {
		parts = append(parts, "floor "+sh.Floor)
	}
	if len(sh.Include) > 0 {
		parts = append(parts, "only on "+joinNames(sh.Include))
	}
	if len(sh.Exclude) > 0 {
		parts = append(parts, "never on "+joinNames(sh.Exclude))
	}

	if dirs.read {
		on, pools := dirs.data[sh.Name], dirs.pools[sh.Name]
		switch {
		case len(on) > 0:
			parts = append(parts, "directory on "+joinNames(on))
		case len(pools) > 0:
			worse(StatusFlag)
			parts = append(parts, "no directory on any matched data disk; it is only on "+joinNames(pools))
		default:
			worse(StatusFlag)
			parts = append(parts, "no directory on any matched data disk")
		}
	}
	r.add(CheckShares, st, sh.Name, "%s.", strings.Join(parts, "; "))
}

// checkCache reports what a re-created cache would lose, from the share configs,
// the capture and the Docker and VM Manager settings.
func checkCache(r *Report, src FlashSource, f *Flash, imp *Import, dirs *dirIndex) error {
	dockerCfg, err := readCfg(r, CheckCache, src, "config/docker.cfg")
	if err != nil {
		return err
	}
	domainCfg, err := readCfg(r, CheckCache, src, "config/domain.cfg")
	if err != nil {
		return err
	}
	docker, domain := dockerCfg.flat(), domainCfg.flat()
	byName := map[string]Share{}
	for _, sh := range imp.Shares {
		byName[sh.Name] = sh
	}

	appdata := "appdata"
	appdataPath := docker["DOCKER_APP_CONFIG_PATH"]
	if p := strings.Split(strings.Trim(appdataPath, "/"), "/"); len(p) >= 3 && p[0] == "mnt" {
		appdata = p[2]
	}
	addAppdataRow(r, byName[appdata], appdata, appdataPath, byName, dirs)

	var names []string
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sh := byName[n]
		if n == appdata {
			continue
		}
		onPool := dirs.pools[n]
		switch {
		case sh.UseCache == "prefer" || sh.UseCache == "only":
			r.add(CheckCache, StatusWarn, n, "The cache setting is %s, so its data lives on the cache, which is re-created. Phase A step 5 must move it to the array.", sh.UseCache)
		case len(onPool) > 0:
			r.add(CheckCache, StatusWarn, n, "It has a directory on %s (cache setting %s), which is re-created. Phase A step 5 must move what is there to the array.", joinNames(onPool), sh.UseCache)
		}
	}

	addLibvirtRow(r, f, domain)
	addDockerRow(r, f, docker)
	return nil
}

// readCfg reads an optional .cfg. An absent file gives nil. One that does not
// parse is a warning naming it, and reads as absent: its settings are not used.
func readCfg(r *Report, check string, src FlashSource, name string) (*cfgFile, error) {
	data, found, err := readOptional(src, name)
	if err != nil || !found {
		return nil, err
	}
	cfg, err := parseCfg(data)
	if err != nil {
		r.add(check, StatusWarn, path.Base(name), "%s does not parse (%v), so none of its settings is read.", name, err)
		return nil, nil
	}
	return cfg, nil
}

func (c *cfgFile) flat() map[string]string {
	if c == nil {
		return map[string]string{}
	}
	return c.section("")
}

func addAppdataRow(r *Report, sh Share, name, appdataPath string, byName map[string]Share, dirs *dirIndex) {
	_, hasCfg := byName[name]
	var parts []string
	st := StatusInfo
	if appdataPath != "" {
		parts = append(parts, fmt.Sprintf("Docker keeps container data under %s", appdataPath))
	}
	if hasCfg {
		parts = append(parts, fmt.Sprintf("cache setting %s", sh.UseCache))
		if sh.UseCache == "prefer" || sh.UseCache == "only" || sh.UseCache == "yes" {
			st = StatusWarn
		}
	} else {
		parts = append(parts, "there is no share config of this name")
	}
	switch {
	case !dirs.read:
		parts = append(parts, "which disks hold its directory is not read by this part of the scan")
	default:
		var holders []string
		holders = append(holders, dirs.data[name]...)
		holders = append(holders, dirs.pools[name]...)
		if len(holders) == 0 {
			parts = append(parts, "no matched disk holds its directory")
		} else {
			parts = append(parts, "its directory is on "+joinNames(holders))
		}
		if len(dirs.pools[name]) > 0 {
			st = StatusWarn
		}
	}
	detail := strings.Join(parts, "; ") + "."
	if st == StatusWarn {
		detail += " Phase A step 5 must move it to the array before the cache is re-created."
	}
	r.add(CheckCache, st, name, "%s", detail)
}

func onCache(location string) bool { return location == "cache" || location == "boot-pool" }

func addLibvirtRow(r *Report, f *Flash, domain map[string]string) {
	if domain["IMAGE_FILE"] == "" {
		return
	}
	p := domain["IMAGE_FILE"]
	vm := ""
	if domain["SERVICE"] == "disable" {
		vm = " VM Manager is disabled."
	}
	switch {
	case f.Capture == nil:
		r.add(CheckCache, StatusWarn, "libvirt.img", "domain.cfg puts it at %s, and without the capture it is not known whether that is on the cache. If it is, Phase A step 5 must move it to the array.%s", p, vm)
	case onCache(f.Capture.LibvirtImgLocation):
		r.add(CheckCache, StatusWarn, "libvirt.img", "It is on the cache (%s). Phase A step 5 must move it to the array; stop the VM service first.%s", p, vm)
	case f.Capture.LibvirtImgLocation == "array":
		r.add(CheckCache, StatusInfo, "libvirt.img", "It is on the array (%s), and is adopted with the data disks.%s", p, vm)
	default:
		r.add(CheckCache, StatusInfo, "libvirt.img", "domain.cfg puts it at %s; the capture did not find it on the array or the cache (%s).%s", p, locationWord(f.Capture.LibvirtImgLocation), vm)
	}
}

func locationWord(l string) string {
	if l == "" {
		return "no location recorded"
	}
	return "recorded as " + l
}

func addDockerRow(r *Report, f *Flash, docker map[string]string) {
	if docker["DOCKER_IMAGE_FILE"] == "" {
		return
	}
	p := docker["DOCKER_IMAGE_FILE"]
	kind := "image file"
	if docker["DOCKER_IMAGE_TYPE"] == "folder" {
		kind = "directory"
	}
	if f.Capture == nil {
		r.add(CheckCache, StatusWarn, "Docker storage", "docker.cfg puts Docker's %s at %s, and without the capture it is not known whether that is on the cache or what each container's writable layer holds.", kind, p)
		return
	}
	d := f.Capture.Docker
	if !onCache(d.DirectoryLocation) {
		r.add(CheckCache, StatusInfo, "Docker storage", "Docker's %s (%s) is not on the cache (%s).", kind, p, locationWord(d.DirectoryLocation))
		return
	}
	detail := fmt.Sprintf("Docker's %s (%s) is on the cache. It is not moved: images are pulled again when containers are recreated. What does not come back is each container's writable layer (doc 04 §5).", kind, p)
	if d.State != "running" {
		r.add(CheckCache, StatusWarn, "Docker storage", "%s Docker was not running at capture, so the writable-layer size per container was not measured: recover anything kept inside a container while Unraid still runs.", detail)
		return
	}
	var sized []WritableLayer
	unmeasured := 0
	for _, l := range d.WritableLayers {
		switch {
		case l.Bytes == nil:
			unmeasured++
		case *l.Bytes > 0:
			sized = append(sized, l)
		}
	}
	sort.Slice(sized, func(i, j int) bool {
		if *sized[i].Bytes != *sized[j].Bytes {
			return *sized[i].Bytes > *sized[j].Bytes
		}
		return sized[i].Container < sized[j].Container
	})
	st := StatusInfo
	if len(sized) > 0 {
		st = StatusWarn
		var list []string
		for _, l := range sized {
			list = append(list, fmt.Sprintf("%s %s", l.Container, formatBytes(*l.Bytes)))
		}
		detail += " Writable layers with data: " + joinNames(list) + "."
	} else {
		detail += " No container's writable layer holds data."
	}
	if unmeasured > 0 {
		detail += fmt.Sprintf(" %d %s could not be measured.", unmeasured, plural(unmeasured, "container", "containers"))
	}
	r.add(CheckCache, st, "Docker storage", "%s", detail)
}

func checkUsers(r *Report, src FlashSource, imp *Import) error {
	imp.Users = []string{}
	data, found, err := readOptional(src, "config/passwd")
	if err != nil {
		return err
	}
	if !found {
		r.add(CheckUsers, StatusWarn, "", "config/passwd was not found, so no user account can be seeded. Accounts are created by hand at the import.")
		return nil
	}
	names, bad := parseUsers(data)
	if names != nil {
		imp.Users = names
	}
	if bad > 0 {
		r.add(CheckUsers, StatusWarn, "", "%d %s in config/passwd could not be read.", bad, plural(bad, "line", "lines"))
	}
	if len(names) == 0 {
		r.add(CheckUsers, StatusInfo, "", "config/passwd lists no user accounts besides the system's.")
		return nil
	}
	r.add(CheckUsers, StatusInfo, "", "%d user %s: %s. Names only are read; passwords cannot be carried over, so each is set again at the import (doc 05 §4 step 4).", len(names), plural(len(names), "account", "accounts"), joinNames(names))
	return nil
}

// checkDocker classes every template by the capture's container list and
// reports the containers that cannot convert.
func checkDocker(r *Report, src FlashSource, f *Flash, imp *Import) error {
	type tmpl struct{ file, name string }
	var parsed []tmpl
	var newest time.Time
	for _, n := range directChildren(src, templatesDir) {
		if !strings.HasSuffix(n, ".xml") {
			continue
		}
		if mt, ok := src.ModTime(n); ok && mt.After(newest) {
			newest = mt
		}
		data, err := src.Read(n)
		if err != nil {
			return err
		}
		name, err := templateName(data)
		if err != nil {
			r.add(CheckTemplates, StatusWarn, path.Base(n), "The template does not parse (%v). It cannot be converted.", err)
			continue
		}
		parsed = append(parsed, tmpl{n, name})
	}

	containers, haveContainers, err := readContainers(r, src, f)
	if err != nil {
		return err
	}
	auto, haveAuto, err := readAutostart(r, src, haveContainers)
	if err != nil {
		return err
	}
	if err := readNetworks(r, src, imp); err != nil {
		return err
	}
	checkStale(r, f, newest)

	dockerMan := map[string]captureContainer{}
	for _, c := range containers {
		if c.origin() == originDockerMan {
			dockerMan[c.Name] = c
		}
	}

	counts := map[TemplateClass]int{}
	nameFiles := map[string][]string{}
	for _, t := range parsed {
		e := TemplateEntry{File: t.file, Name: t.name, Class: ClassUnknown}
		if haveContainers {
			c, ok := dockerMan[t.name]
			switch {
			case !ok:
				e.Class = ClassTemplateOnly
			case haveAuto && auto[t.name].position > 0:
				e.Class = ClassAutostart
				e.AutostartPosition, e.AutostartWaitSeconds = auto[t.name].position, auto[t.name].wait
			case c.running():
				e.Class = ClassRunning
			default:
				e.Class = ClassStopped
			}
		}
		counts[e.Class]++
		nameFiles[t.name] = append(nameFiles[t.name], path.Base(t.file))
		imp.Templates = append(imp.Templates, e)
	}
	sort.Slice(imp.Templates, func(i, j int) bool { return imp.Templates[i].File < imp.Templates[j].File })

	total := len(parsed)
	switch {
	case total == 0 && len(directChildren(src, templatesDir)) == 0:
		r.add(CheckTemplates, StatusInfo, "", "No Docker templates were found in config/plugins/dockerMan/templates-user/.")
	case !haveContainers:
		r.add(CheckTemplates, StatusInfo, "", "%d %s parsed and every one is classed unknown: without the container list nothing says which are installed, so the preview pre-selects none.", total, plural(total, "template", "templates"))
	default:
		installed := counts[ClassAutostart] + counts[ClassRunning] + counts[ClassStopped]
		r.add(CheckTemplates, StatusInfo, "", "%d %s parsed: %d autostart, %d running, %d stopped, %d template only. %d %s installed; a template with no container is a record of an app once installed.",
			total, plural(total, "template", "templates"), counts[ClassAutostart], counts[ClassRunning], counts[ClassStopped], counts[ClassTemplateOnly], installed, plural(installed, "is", "are"))
	}
	var dupes []string
	for name, files := range nameFiles {
		if len(files) > 1 {
			sort.Strings(files)
			dupes = append(dupes, fmt.Sprintf("%s (%s)", name, joinNames(files)))
		}
	}
	sort.Strings(dupes)
	for _, d := range dupes {
		r.add(CheckTemplates, StatusWarn, "", "More than one template names the same container: %s. Each is classed by that one container.", d)
	}

	return checkContainers(r, src, containers, haveContainers, dockerMan, nameFiles, imp)
}

func checkStale(r *Report, f *Flash, newest time.Time) {
	if f.Capture == nil || f.Capture.CapturedAt == "" || newest.IsZero() {
		return
	}
	at, err := time.Parse(time.RFC3339, f.Capture.CapturedAt)
	if err != nil {
		r.add(CheckCapture, StatusWarn, "", "The capture's time (%q) is not a timestamp, so it is not known whether the capture is older than the templates.", f.Capture.CapturedAt)
		return
	}
	if at.Before(newest) {
		r.add(CheckCapture, StatusWarn, "", "The capture was taken %s, but a template on the flash was saved later (%s), so the capture may be stale. Run the prepare script again and take a new Flash Backup. (A flash carries no time zone, so a gap of hours can be that.)", at.UTC().Format(time.RFC3339), newest.UTC().Format(time.RFC3339))
	}
}

func readContainers(r *Report, src FlashSource, f *Flash) ([]captureContainer, bool, error) {
	const name = captureDir + "/containers.json"
	data, found, err := readOptional(src, name)
	if err != nil {
		return nil, false, err
	}
	switch {
	case !found && f.Capture == nil && f.CaptureProblem == "":
		r.add(CheckContainers, StatusWarn, "", "The Phase A capture was not found in this source, so no container is known and every template is classed unknown. Run the prepare script on the Unraid server (Phase A step 0), then take the Flash Backup again.")
		return nil, false, nil
	case !found && f.Capture != nil && f.Capture.Docker.State != "" && f.Capture.Docker.State != "running":
		r.add(CheckContainers, StatusWarn, "", "The capture says Docker was %s when it was taken, so there is no container list and every template is classed unknown. Start Docker on the Unraid server and run the prepare script again (Phase A step 0).", f.Capture.Docker.State)
		return nil, false, nil
	case !found:
		r.add(CheckContainers, StatusWarn, "", "containers.json is not in the capture, so no container is known and every template is classed unknown. Run the prepare script again (Phase A step 0).")
		return nil, false, nil
	}
	containers, err := parseContainers(data)
	if err != nil {
		r.add(CheckContainers, StatusWarn, "containers.json", "The file does not parse (%v), so no container is known and every template is classed unknown. Run the prepare script again (Phase A step 0).", err)
		return nil, false, nil
	}
	return containers, true, nil
}

func readAutostart(r *Report, src FlashSource, haveContainers bool) (map[string]autostartEntry, bool, error) {
	data, found, err := readOptional(src, captureDir+"/autostart")
	if err != nil || !found {
		if err == nil && haveContainers {
			r.add(CheckContainers, StatusInfo, "", "The capture has no autostart list (Unraid had none), so no container is classed autostart.")
		}
		return nil, false, err
	}
	entries, bad := parseAutostart(data)
	if bad > 0 {
		r.add(CheckContainers, StatusWarn, "autostart", "%d %s of the autostart list could not be read; the containers on them are not classed autostart.", bad, plural(bad, "line", "lines"))
	}
	return entries, true, nil
}

func readNetworks(r *Report, src FlashSource, imp *Import) error {
	const name = captureDir + "/networks.json"
	imp.Networks = []Network{}
	data, found, err := readOptional(src, name)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	nets, err := parseNetworks(data)
	if err != nil {
		r.add(CheckContainers, StatusWarn, "networks.json", "The file does not parse (%v), so the Docker networks are not known to the converter.", err)
		return nil
	}
	imp.Networks = nets
	var custom []string
	for _, n := range nets {
		if !builtinNetwork(n.Name) {
			custom = append(custom, n.Name)
		}
	}
	sort.Strings(custom)
	if len(custom) == 0 {
		r.add(CheckContainers, StatusInfo, "", "%d Docker %s captured, none user-defined.", len(nets), plural(len(nets), "network", "networks"))
	} else {
		r.add(CheckContainers, StatusInfo, "", "%d Docker %s captured; user-defined: %s.", len(nets), plural(len(nets), "network", "networks"), joinNames(custom))
	}
	return nil
}

func checkContainers(r *Report, src FlashSource, containers []captureContainer, have bool, dockerMan map[string]captureContainer, templated map[string][]string, imp *Import) error {
	projects := map[string]*ComposeProject{}
	for _, n := range src.List(composeDir) {
		rel := strings.TrimPrefix(n, composeDir+"/")
		dir, file, ok := strings.Cut(rel, "/")
		if ok && file == "compose.yaml" {
			projects[dir] = &ComposeProject{Name: dir, File: n}
		}
	}
	var noTemplate, byHand []string
	counts := map[containerOrigin]int{}
	for _, c := range containers {
		counts[c.origin()]++
		switch c.origin() {
		case originDockerMan:
			if len(templated[c.Name]) == 0 {
				noTemplate = append(noTemplate, c.Name)
			}
		case originCompose:
			p := c.Config.Labels[labelCompose]
			if projects[p] == nil {
				projects[p] = &ComposeProject{Name: p}
			}
			projects[p].Containers = append(projects[p].Containers, c.Name)
		default:
			byHand = append(byHand, c.Name)
		}
	}
	sort.Strings(noTemplate)
	sort.Strings(byHand)

	var names []string
	for n := range projects {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := projects[n]
		sort.Strings(p.Containers)
		imp.ComposeProjects = append(imp.ComposeProjects, *p)
	}

	if have {
		r.add(CheckContainers, StatusInfo, "", "%d %s in the capture: %d from the Docker page (dockerMan), %d from Compose Manager, %d created by hand.",
			len(containers), plural(len(containers), "container", "containers"), counts[originDockerMan], counts[originCompose], counts[originByHand])
	}
	for _, c := range noTemplate {
		r.add(CheckContainers, StatusFlag, c, "A dockerMan container with no template whose <Name> matches. It cannot be converted: open it on the Docker page, edit it and apply to save its template, then run the prepare script again (doc 05 §4 step 2).")
	}
	for _, c := range byHand {
		r.add(CheckContainers, StatusFlag, c, "Created by hand (docker run), so it has no template to convert. Recreate it from its run command.")
	}
	for _, p := range imp.ComposeProjects {
		switch {
		case p.File == "":
			r.add(CheckContainers, StatusWarn, p.Name, "Compose Manager project (%s) whose compose.yaml is not in the source, so it cannot be offered.", joinNames(p.Containers))
		case len(p.Containers) == 0:
			r.add(CheckContainers, StatusInfo, p.Name, "Compose Manager project with a compose.yaml and no container in the capture. It is offered as its own compose.yaml.")
		default:
			r.add(CheckContainers, StatusInfo, p.Name, "Compose Manager project (%s), offered as its own compose.yaml (doc 05 §4 step 19).", joinNames(p.Containers))
		}
	}
	return nil
}

type userScript struct {
	dir  string
	name string
}

func checkUserScripts(r *Report, src FlashSource) error {
	cronData, haveCron, err := readOptional(src, userScriptsCron)
	if err != nil {
		return err
	}
	cron := map[string]string{}
	if haveCron {
		var unreadable []int
		cron, unreadable = parseCustomCron(cronData)
		for _, n := range unreadable {
			r.add(CheckUserScripts, StatusWarn, "customSchedule.cron", "Line %d of customSchedule.cron is not the User Scripts plugin's schedule and script, so a script may be scheduled, or another command run, without this report saying so. The line is not quoted: open the file on the Unraid server to read it.", n)
		}
	}
	var scripts []userScript
	seen := map[string]bool{}
	for _, n := range src.List(userScriptsDir) {
		dir, file, ok := strings.Cut(strings.TrimPrefix(n, userScriptsDir+"/"), "/")
		if !ok || file != "script" || seen[dir] {
			continue
		}
		seen[dir] = true
		name := dir
		if data, found, err := readOptional(src, userScriptsDir+"/"+dir+"/name"); err != nil {
			return err
		} else if found {
			if s := strings.TrimSpace(string(data)); s != "" {
				name = s
			}
		}
		scripts = append(scripts, userScript{dir: dir, name: name})
	}
	sort.Slice(scripts, func(i, j int) bool { return scripts[i].dir < scripts[j].dir })

	pluginPresent := haveCron
	for _, n := range directChildren(src, "config/plugins") {
		if path.Base(n) == "user.scripts.plg" {
			pluginPresent = true
		}
	}
	if len(scripts) == 0 {
		if pluginPresent {
			r.add(CheckUserScripts, StatusWarn, "", "The User Scripts plugin is installed, but no entry was found under config/plugins/user.scripts/scripts/. This is not a finding that there are no scripts: look at the plugin's page on the Unraid server and list them by hand.")
		} else {
			r.add(CheckUserScripts, StatusInfo, "", "No User Scripts plugin files were found under config/plugins/user.scripts/. If the plugin is in use, its entries are not where this scan looks.")
		}
	} else {
		r.add(CheckUserScripts, StatusInfo, "", "%d User Scripts %s found. They are listed, never executed or translated (Q83): recreate what is still wanted as a cron job or systemd timer (doc 05 §4 step 24).", len(scripts), plural(len(scripts), "entry", "entries"))
	}
	for _, s := range scripts {
		if sched, ok := cron[s.dir]; ok {
			r.add(CheckUserScripts, StatusInfo, s.name, "Scheduled in customSchedule.cron (%s), so it runs while enabled.", sched)
		} else {
			r.add(CheckUserScripts, StatusInfo, s.name, "No schedule in customSchedule.cron. Whether the plugin runs it another way (at array start or stop, or on demand) is not shown by the files this scan reads.")
		}
	}
	var stray []string
	for dir := range cron {
		if !seen[dir] {
			stray = append(stray, dir)
		}
	}
	sort.Strings(stray)
	for _, dir := range stray {
		r.add(CheckUserScripts, StatusWarn, dir, "customSchedule.cron schedules a script of this name, and no entry of that name was found.")
	}
	return nil
}

const parityChecklist = "The final parity check cannot be confirmed from this source; run one on Unraid and confirm it completes clean before cutover (the pre-cutover checklist, doc 05 §3)."

func checkParityHistory(r *Report, src FlashSource, now time.Time) error {
	var last *parityCheckEntry
	source := "the capture's var.ini"
	running := false

	data, found, err := readOptional(src, captureDir+"/var.ini")
	if err != nil {
		return err
	}
	if found {
		cfg, perr := parseCfg(data)
		if perr != nil {
			r.add(CheckParityHistory, StatusWarn, "var.ini", "The capture's var.ini does not parse (%v), so the parity-check fields are not read from it.", perr)
		} else {
			v := cfg.section("")
			running = v["mdResync"] != "" && v["mdResync"] != "0"
			if sec, err := strconv.ParseInt(v["sbSynced"], 10, 64); err == nil && sec > 0 {
				exit, e1 := strconv.Atoi(v["sbSyncExit"])
				errs, e2 := strconv.Atoi(v["sbSyncErrs"])
				if e1 != nil || e2 != nil {
					r.add(CheckParityHistory, StatusWarn, "var.ini", "The capture's var.ini gives a last parity check without a readable exit code or error count, so the result is not read from it.")
				} else {
					last = &parityCheckEntry{at: time.Unix(sec, 0).UTC(), exit: exit, errs: errs}
				}
			}
		}
	}
	if running {
		r.add(CheckParityHistory, StatusWarn, "", "A parity check or sync was still running when the capture was taken. Let it finish and confirm it completes clean before cutover.")
	}
	if last == nil {
		logData, found, err := readOptional(src, parityChecksLog)
		if err != nil {
			return err
		}
		if found {
			var bad int
			last, bad = parseParityLog(logData)
			source = "config/parity-checks.log"
			if bad > 0 {
				r.add(CheckParityHistory, StatusWarn, "parity-checks.log", "%d %s of the parity-check log could not be read.", bad, plural(bad, "line", "lines"))
			}
		}
	}
	if last == nil {
		r.add(CheckParityHistory, StatusWarn, "", "No parity-check history was found in the capture's var.ini or config/parity-checks.log. %s", parityChecklist)
		return nil
	}
	age := now.Sub(last.at)
	when := last.at.UTC().Format("2006-01-02")
	var problems []string
	if last.exit != 0 || last.errs != 0 {
		problems = append(problems, fmt.Sprintf("it was not clean (exit code %d, %d %s)", last.exit, last.errs, plural(last.errs, "error", "errors")))
	}
	if age > parityCheckLimit {
		problems = append(problems, fmt.Sprintf("it is %d days old", int(age.Hours()/24)))
	}
	if len(problems) > 0 {
		r.add(CheckParityHistory, StatusWarn, "", "The last parity check, on %s (from %s): %s. Migrating on a degraded array risks everything: run a clean check first (Phase A step 6).", when, source, strings.Join(problems, " and "))
		return nil
	}
	r.add(CheckParityHistory, StatusPass, "", "The last parity check, on %s (from %s), completed clean with 0 errors.", when, source)
	return nil
}

var knownPlugins = map[string]string{
	"user.scripts":    "its scripts are listed, never executed or translated (Q83)",
	"compose.manager": "its projects are offered as their own compose.yaml (doc 05 §4 step 19)",
	"ca.backup2":      "Hoserva has appdata backup (doc 10)",
	"appdata.backup":  "Hoserva has appdata backup (doc 10)",
}

func checkPlugins(r *Report, src FlashSource) error {
	var others []string
	count := 0
	for _, n := range directChildren(src, "config/plugins") {
		if !strings.HasSuffix(n, ".plg") {
			continue
		}
		count++
		name := strings.TrimSuffix(path.Base(n), ".plg")
		if what, ok := knownPlugins[name]; ok {
			r.add(CheckPlugins, StatusInfo, name, "Installed. Hoserva counterpart: %s.", what)
		} else {
			others = append(others, name)
		}
	}
	sort.Strings(others)
	switch {
	case count == 0:
		r.add(CheckPlugins, StatusInfo, "", "No plugin files (config/plugins/*.plg) were found.")
	case len(others) > 0:
		r.add(CheckPlugins, StatusInfo, "", "%d other %s with no known Hoserva counterpart: %s.", len(others), plural(len(others), "plugin", "plugins"), joinNames(others))
	}
	return nil
}

func isComment(line string) bool { return strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") }

func checkCustomConfig(r *Report, src FlashSource) error {
	if data, found, err := readOptional(src, "config/smb-extra.conf"); err != nil {
		return err
	} else if found {
		n := 0
		for _, l := range trimmedLines(data) {
			if !isComment(l) {
				n++
			}
		}
		if n > 0 {
			r.add(CheckCustomConfig, StatusWarn, "smb-extra.conf", "%d %s of custom Samba configuration. It is not imported: look at the lines on the Unraid server and recreate any that are still wanted.", n, plural(n, "line", "lines"))
		}
	}
	if data, found, err := readOptional(src, "config/go"); err != nil {
		return err
	} else if found {
		n := 0
		for _, l := range trimmedLines(data) {
			if isComment(l) || strings.HasPrefix(l, "/usr/local/sbin/emhttp") {
				continue
			}
			n++
		}
		if n > 0 {
			r.add(CheckCustomConfig, StatusWarn, "go", "%d custom %s in config/go, which runs at Unraid's boot. They are not imported or executed: recreate what is still wanted on the new host.", n, plural(n, "line", "lines"))
		}
	}
	return nil
}

func checkSettings(r *Report, src FlashSource, imp *Import) error {
	s := &imp.Schedules

	shareCfg, err := readCfg(r, CheckSettings, src, "config/share.cfg")
	if err != nil {
		return err
	}
	s.MoverCron = shareCfg.flat()["shareMoverSchedule"]
	if s.MoverCron == "" {
		r.add(CheckSettings, StatusInfo, "mover schedule", "Not available from config/share.cfg: set the mover schedule by hand.")
	} else {
		r.add(CheckSettings, StatusInfo, "mover schedule", "%s. It can be offered as Hoserva's mover schedule.", s.MoverCron)
	}

	dynamix, err := readCfg(r, CheckSettings, src, dynamixCfg)
	if err != nil {
		return err
	}
	if dynamix != nil {
		if _, ok := dynamix.values["parity"]; ok {
			p := dynamix.section("parity")
			s.ParityCheck = ParityCheckSchedule{
				Found: true, Mode: p["mode"], Hour: p["hour"], DayOfMonth: p["dotm"], Day: p["day"], Month: p["month"], Frequency: p["frequency"],
				Correcting: p["write"] != "NOCORRECT",
			}
		}
	}
	if !s.ParityCheck.Found {
		r.add(CheckSettings, StatusInfo, "parity-check schedule", "Not available from config/plugins/dynamix/dynamix.cfg: set the scrub schedule by hand.")
	} else {
		mode := "correcting"
		if !s.ParityCheck.Correcting {
			mode = "non-correcting"
		}
		r.add(CheckSettings, StatusInfo, "parity-check schedule", "Unraid's schedule (mode %q, time %q, %s). It can be offered as the scrub schedule; a non-correcting check maps to a scrub that only reports.", s.ParityCheck.Mode, s.ParityCheck.Hour, mode)
	}

	diskCfg, err := readCfg(r, CheckSettings, src, "config/disk.cfg")
	if err != nil {
		return err
	}
	s.SpindownDelay = diskCfg.flat()["spindownDelay"]
	if s.SpindownDelay == "" {
		r.add(CheckSettings, StatusInfo, "spin-down delay", "Not available from config/disk.cfg: set the default by hand.")
	} else {
		r.add(CheckSettings, StatusInfo, "spin-down delay", "Unraid's global setting is %q. It can be offered as Hoserva's default.", s.SpindownDelay)
	}

	seen := map[string]bool{}
	for _, n := range directChildren(src, notifyAgentsDir) {
		name := strings.TrimSuffix(path.Base(n), path.Ext(n))
		if name != "" && !seen[name] {
			seen[name] = true
			s.NotifyAgents = append(s.NotifyAgents, name)
		}
	}
	sort.Strings(s.NotifyAgents)
	if len(s.NotifyAgents) == 0 {
		r.add(CheckSettings, StatusInfo, "notification agents", "None found under config/plugins/dynamix/notifications/agents/. Set up notification channels by hand (doc 05 §4 step 23).")
	} else {
		r.add(CheckSettings, StatusInfo, "notification agents", "%s. Only the names are read, and no secret is carried over: recreate each as a channel (doc 05 §4 step 23).", joinNames(s.NotifyAgents))
	}
	return nil
}

// inventory runs the checks that describe what the Unraid server was
// configured to do, filling in the import model as it goes.
func (s *Scanner) inventory(ctx context.Context, r *Report, src FlashSource, f *Flash, members []*member, now time.Time) error {
	dirs, err := buildDirIndex(ctx, s.Dirs, members)
	if err != nil {
		return err
	}
	for _, step := range []func() error{
		func() error { return checkShares(r, src, &r.Import, dirs) },
		func() error { return checkCache(r, src, f, &r.Import, dirs) },
		func() error { return checkUsers(r, src, &r.Import) },
		func() error { return checkDocker(r, src, f, &r.Import) },
		func() error { return checkUserScripts(r, src) },
		func() error { return checkParityHistory(r, src, now) },
		func() error { return checkPlugins(r, src) },
		func() error { return checkCustomConfig(r, src) },
		func() error { return checkSettings(r, src, &r.Import) },
	} {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}
