package template

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// unraidXML wraps a template body in the document every test shares.
func unraidXML(body string) []byte {
	return []byte(`<?xml version="1.0"?>
<Container version="2">
  <Name>app</Name>
  <Repository>registry.example.com/team/app:1.2.3</Repository>
` + body + `
</Container>`)
}

func convertOK(t *testing.T, body string) (*Conversion, map[string]any) {
	t.Helper()
	c, err := ConvertUnraid(unraidXML(body), ConvertOptions{})
	if err != nil {
		t.Fatalf("ConvertUnraid: %v", err)
	}
	return c, serviceOf(t, c)
}

func serviceOf(t *testing.T, c *Conversion) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(c.Compose), &doc); err != nil {
		t.Fatalf("generated Compose is not YAML: %v\n%s", err, c.Compose)
	}
	services, _ := doc["services"].(map[string]any)
	if len(services) != 1 {
		t.Fatalf("services = %v, want exactly one\n%s", services, c.Compose)
	}
	for _, v := range services {
		svc, _ := v.(map[string]any)
		return svc
	}
	return nil
}

func warningsOf(c *Conversion, class string) []Warning {
	var out []Warning
	for _, w := range c.Warnings {
		if w.Class == class {
			out = append(out, w)
		}
	}
	return out
}

func hasWarning(c *Conversion, class, detailPart string) bool {
	for _, w := range warningsOf(c, class) {
		if strings.Contains(w.Detail, detailPart) || strings.Contains(w.Message, detailPart) {
			return true
		}
	}
	return false
}

func strs(t *testing.T, v any) []string {
	t.Helper()
	list, _ := v.([]any)
	out := make([]string, len(list))
	for i, e := range list {
		out[i] = fmt.Sprint(e)
	}
	return out
}

const fullTemplate = `<?xml version="1.0"?>
<Container version="2">
  <Name>Media Server</Name>
  <Repository>lscr.io/linuxserver/jellyfin:10.10.7</Repository>
  <Registry>https://example.com/registry</Registry>
  <Network>bridge</Network>
  <MyIP/>
  <Shell>bash</Shell>
  <Privileged>false</Privileged>
  <Support>https://example.com/support</Support>
  <Project>https://example.com/project</Project>
  <Overview>Streams media.</Overview>
  <Category>MediaServer:Video</Category>
  <WebUI>http://[IP]:[PORT:8096]/</WebUI>
  <Icon>https://example.com/icon.png</Icon>
  <ExtraParams>--restart=unless-stopped --memory=2g</ExtraParams>
  <PostArgs>--verbose "two words"</PostArgs>
  <CPUset>0-3</CPUset>
  <DonateLink>https://example.com/donate</DonateLink>
  <Requires>A GPU is optional.</Requires>
  <Config Name="WebUI" Target="8096" Default="8096" Mode="tcp" Description="Web port" Type="Port" Display="always" Required="true" Mask="false">8096</Config>
  <Config Name="Discovery" Target="7359" Default="" Mode="udp" Description="" Type="Port" Display="advanced" Required="false" Mask="false">7359</Config>
  <Config Name="Config" Target="/config" Default="/mnt/user/appdata/jellyfin" Mode="rw" Description="" Type="Path" Display="always" Required="true" Mask="false">/mnt/user/appdata/jellyfin</Config>
  <Config Name="Media" Target="/data" Default="" Mode="ro" Description="" Type="Path" Display="always" Required="true" Mask="false">/mnt/user/media</Config>
  <Config Name="PUID" Target="PUID" Default="99" Mode="" Description="User ID" Type="Variable" Display="advanced" Required="false" Mask="false">99</Config>
  <Config Name="PGID" Target="PGID" Default="100" Mode="" Description="Group ID" Type="Variable" Display="advanced" Required="false" Mask="false">100</Config>
  <Config Name="Render" Target="/dev/dri" Default="" Mode="" Description="" Type="Device" Display="advanced" Required="false" Mask="false">/dev/dri</Config>
  <Config Name="Tag" Target="org.example.tag" Default="" Mode="" Description="" Type="Label" Display="advanced" Required="false" Mask="false">media</Config>
</Container>`

func TestConvertUnraid_MapsEveryFieldOfTheFieldMapping(t *testing.T) {
	c, err := ConvertUnraid([]byte(fullTemplate), ConvertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	svc := serviceOf(t, c)

	want := map[string]any{
		"image":          "lscr.io/linuxserver/jellyfin:10.10.7",
		"container_name": "Media-Server",
		"network_mode":   "bridge",
		"privileged":     false,
		"restart":        "unless-stopped",
		"mem_limit":      "2g",
		"cpuset":         "0-3",
	}
	for k, v := range want {
		if !reflect.DeepEqual(svc[k], v) {
			t.Errorf("%s = %#v, want %#v", k, svc[k], v)
		}
	}
	if got := strs(t, svc["ports"]); !reflect.DeepEqual(got, []string{"8096:8096/tcp", "7359:7359/udp"}) {
		t.Errorf("ports = %v", got)
	}
	if got := strs(t, svc["volumes"]); !reflect.DeepEqual(got, []string{"/mnt/user/appdata/jellyfin:/config", "/mnt/user/media:/data:ro"}) {
		t.Errorf("volumes = %v", got)
	}
	if got := svc["environment"]; !reflect.DeepEqual(got, map[string]any{"PUID": "99", "PGID": "100"}) {
		t.Errorf("environment = %#v", got)
	}
	if got := strs(t, svc["devices"]); !reflect.DeepEqual(got, []string{"/dev/dri:/dev/dri"}) {
		t.Errorf("devices = %v", got)
	}
	if got := svc["labels"]; !reflect.DeepEqual(got, map[string]any{"org.example.tag": "media"}) {
		t.Errorf("labels = %#v", got)
	}
	if got := strs(t, svc["command"]); !reflect.DeepEqual(got, []string{"--verbose", "two words"}) {
		t.Errorf("command = %v", got)
	}

	m := c.Metadata
	if m.Title != "Media Server" || m.Overview != "Streams media." || m.Category != "MediaServer:Video" ||
		m.WebUI != "http://[IP]:[PORT:8096]/" || m.Icon != "https://example.com/icon.png" ||
		m.Support != "https://example.com/support" || m.Project != "https://example.com/project" ||
		m.Requires != "A GPU is optional." || m.DonateLink != "https://example.com/donate" {
		t.Errorf("metadata = %+v", m)
	}
	wantVars := []UnraidVariable{{Name: "PUID", Value: "99", Description: "User ID"}, {Name: "PGID", Value: "100", Description: "Group ID"}}
	if !reflect.DeepEqual(m.Variables, wantVars) {
		t.Errorf("variables = %+v, want %+v", m.Variables, wantVars)
	}
	if !c.Clean() {
		t.Errorf("a template with no untranslated part is not clean: %+v", c.Warnings)
	}
	if !hasWarning(c, WarnNote, "<Shell>") {
		t.Errorf("a dropped <Shell> has no note: %+v", c.Warnings)
	}
}

// The data-loss scenario: a conversion that silently loses a flag, volume or
// privilege. Everything not translated is reported as a warning and, for
// ExtraParams, written in the Compose file.
func TestConvertUnraid_NothingIsDroppedSilently(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		class  string
		detail string
		// inComment is set when the text must be listed in the Compose
		// file's comment.
		inComment string
	}{
		{"unknown long flag", `<ExtraParams>--some-exotic-flag=value</ExtraParams>`, WarnUntranslatedFlag, "--some-exotic-flag=value", "--some-exotic-flag=value"},
		{"unknown flag with a separate value", `<ExtraParams>--some-exotic-flag value</ExtraParams>`, WarnUntranslatedFlag, "--some-exotic-flag value", "--some-exotic-flag value"},
		{"unknown short flag", `<ExtraParams>-d</ExtraParams>`, WarnUntranslatedFlag, "-d", "-d"},
		{"privileged as a flag", `<ExtraParams>--privileged</ExtraParams>`, WarnUntranslatedFlag, "--privileged", "--privileged"},
		{"label flag", `<ExtraParams>--label com.example=1</ExtraParams>`, WarnUntranslatedFlag, "--label", "--label com.example=1"},
		{"a word that is not a flag", `<ExtraParams>stray</ExtraParams>`, WarnUntranslatedFlag, "stray", "stray"},
		{"flag with a bad value", `<ExtraParams>--memory=lots</ExtraParams>`, WarnUntranslatedFlag, "--memory=lots", "--memory=lots"},
		{"flag with no value", `<ExtraParams>--memory</ExtraParams>`, WarnUntranslatedFlag, "--memory", "--memory"},
		{"env inherited from the host", `<ExtraParams>-e SECRET</ExtraParams>`, WarnUntranslatedFlag, "-e SECRET", "-e SECRET"},
		{"relative volume source", `<ExtraParams>-v ./data:/data</ExtraParams>`, WarnUntranslatedFlag, "./data:/data", "-v ./data:/data"},
		{"shell syntax", `<ExtraParams>--restart=always; touch /tmp/x</ExtraParams>`, WarnUntranslatedFlag, "--restart=always;", ""},
		{"shell expansion", `<ExtraParams>--dns=$DNS</ExtraParams>`, WarnUntranslatedFlag, "--dns=$DNS", ""},
		{"unterminated quote", `<ExtraParams>--health-cmd='curl</ExtraParams>`, WarnUntranslatedFlag, "--health-cmd", ""},
		{"unknown field", `<Gizmo>on</Gizmo>`, WarnUntranslatedField, "on", ""},
		{"unknown config type", `<Config Type="Gadget" Target="x">1</Config>`, WarnUntranslatedField, "Gadget", ""},
		{"bad privileged value", `<Privileged>maybe</Privileged>`, WarnUntranslatedField, "maybe", ""},
		{"post args with shell syntax", `<PostArgs>run &amp;&amp; stop</PostArgs>`, WarnUntranslatedField, "run && stop", ""},
		{"relative host path", `<Config Type="Path" Target="/data" Mode="rw">data</Config>`, WarnFlaggedPath, "data", ""},
		{"bad port", `<Config Type="Port" Target="80" Mode="tcp">http</Config>`, WarnUntranslatedField, "http", ""},
		{"unknown mode", `<Config Type="Path" Target="/data" Mode="sideways">/mnt/user/x</Config>`, WarnUntranslatedField, "sideways", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, svc := convertOK(t, tc.body)
			if !hasWarning(c, tc.class, tc.detail) {
				t.Errorf("no %s warning mentioning %q; warnings: %+v", tc.class, tc.detail, c.Warnings)
			}
			if c.Clean() {
				t.Error("a conversion that dropped something is reported clean")
			}
			if tc.inComment != "" && !strings.Contains(c.Compose, "#   "+tc.inComment+"\n") {
				t.Errorf("%q is not listed in the Compose comment:\n%s", tc.inComment, c.Compose)
			}
			for _, key := range []string{"privileged", "volumes", "ports"} {
				if _, ok := svc[key]; ok && tc.name != "relative host path" {
					t.Errorf("%s was set by a template part that could not be translated: %v", key, svc[key])
				}
			}
		})
	}
}

func TestConvertUnraid_ComposeCommentIsTheDocumentedFormat(t *testing.T) {
	c, _ := convertOK(t, `<ExtraParams>--some-exotic-flag=value</ExtraParams>`)
	want := "  # Hoserva: could not translate the following Unraid ExtraParams:\n" +
		"  #   --some-exotic-flag=value\n" +
		"  # Review and add the Compose equivalent manually if required.\n  app:\n"
	if !strings.Contains(c.Compose, want) {
		t.Errorf("Compose lacks the documented comment, got:\n%s", c.Compose)
	}
}

func TestConvertUnraid_ARelativePathIsNeverEmittedAsAMount(t *testing.T) {
	c, svc := convertOK(t, `<Config Type="Path" Target="/data" Mode="rw">appdata</Config>`)
	if _, ok := svc["volumes"]; ok {
		t.Errorf("a relative path became a mount, which Compose reads as a named volume: %v", svc["volumes"])
	}
	if _, ok := yamlTop(t, c)["volumes"]; ok {
		t.Error("a named volume was declared for a relative host path")
	}
}

func yamlTop(t *testing.T, c *Conversion) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(c.Compose), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestConvertUnraid_FlaggedPaths(t *testing.T) {
	flagged := []string{"/boot/config", "/mnt/disks/usb1/media", "/mnt/user0/media", "/mnt/disk1/media", "/mnt/pool2/data", "/var/run/docker.sock", "/mnt/cache"}
	for _, p := range flagged {
		t.Run(p, func(t *testing.T) {
			for name, body := range map[string]string{
				"Config":      fmt.Sprintf(`<Config Type="Path" Target="/x" Mode="rw">%s</Config>`, p),
				"ExtraVolume": fmt.Sprintf(`<ExtraParams>-v %s:/x</ExtraParams>`, p),
				"ExtraMount":  fmt.Sprintf(`<ExtraParams>--mount type=bind,src=%s,dst=/x</ExtraParams>`, p),
			} {
				c, svc := convertOK(t, body)
				if !hasWarning(c, WarnFlaggedPath, p) {
					t.Errorf("%s: %s is not flagged: %+v", name, p, c.Warnings)
				}
				if c.Clean() {
					t.Errorf("%s: a flagged path leaves the conversion clean", name)
				}
				text := fmt.Sprint(svc["volumes"])
				if !strings.Contains(text, p) {
					t.Errorf("%s: the flagged path was rewritten or dropped: %s", name, text)
				}
			}
		})
	}
	for _, p := range []string{"/mnt/user/media", "/mnt/user", "/mnt/cache/appdata/x"} {
		c, _ := convertOK(t, fmt.Sprintf(`<Config Type="Path" Target="/x" Mode="rw">%s</Config>`, p))
		if len(warningsOf(c, WarnFlaggedPath)) != 0 {
			t.Errorf("%s is flagged but maps identically: %+v", p, c.Warnings)
		}
	}
}

func TestConvertUnraid_ExtraParamsLongAndShortFormsAreOneFlag(t *testing.T) {
	forms := []string{
		"--memory=512m", "--memory 512m", "-m512m", "-m=512m", "-m 512m",
	}
	for _, f := range forms {
		_, svc := convertOK(t, "<ExtraParams>"+f+"</ExtraParams>")
		if svc["mem_limit"] != "512m" {
			t.Errorf("%q: mem_limit = %v, want 512m", f, svc["mem_limit"])
		}
	}
	for _, f := range []string{"-it", "-ti", "-i -t", "--interactive --tty", "--interactive=true --tty=true"} {
		_, svc := convertOK(t, "<ExtraParams>"+f+"</ExtraParams>")
		if svc["stdin_open"] != true || svc["tty"] != true {
			t.Errorf("%q: stdin_open=%v tty=%v, want both true", f, svc["stdin_open"], svc["tty"])
		}
	}
	for _, f := range []string{"-u 99:100", "--user=99:100", "-u=99:100", "-u99:100"} {
		if _, svc := convertOK(t, "<ExtraParams>"+f+"</ExtraParams>"); svc["user"] != "99:100" {
			t.Errorf("%q: user = %v", f, svc["user"])
		}
	}
	for _, f := range []string{"-h box", "--hostname box", "--hostname=box"} {
		if _, svc := convertOK(t, "<ExtraParams>"+f+"</ExtraParams>"); svc["hostname"] != "box" {
			t.Errorf("%q: hostname = %v", f, svc["hostname"])
		}
	}
	if _, svc := convertOK(t, "<ExtraParams>-w /srv</ExtraParams>"); svc["working_dir"] != "/srv" {
		t.Errorf("-w: working_dir = %v", svc["working_dir"])
	}
}

func TestConvertUnraid_TranslateTable(t *testing.T) {
	cases := []struct {
		flags string
		check func(t *testing.T, svc map[string]any)
	}{
		{"--restart=on-failure:3", eq("restart", "on-failure:3")},
		{"--memory-swap=1g", eq("memswap_limit", "1g")},
		{"--memory-swap=-1", eq("memswap_limit", -1)},
		{"--cpus=1.5", eq("cpus", 1.5)},
		{"--pids-limit=100", eq("pids_limit", 100)},
		{"--workdir=/srv", eq("working_dir", "/srv")},
		{"--group-add=video --group-add=100", eqList("group_add", "video", "100")},
		{"--entrypoint='/opt/my app/start'", eqList("entrypoint", "/opt/my app/start")},
		{"--entrypoint=", func(t *testing.T, svc map[string]any) {
			if got, ok := svc["entrypoint"].([]any); !ok || len(got) != 0 {
				t.Errorf("entrypoint = %#v, want an empty list", svc["entrypoint"])
			}
		}},
		{"--init", eq("init", true)},
		{"--init=false", eq("init", false)},
		{"--read-only", eq("read_only", true)},
		{"--device=/dev/ttyUSB0:/dev/ttyUSB0:rwm", eqList("devices", "/dev/ttyUSB0:/dev/ttyUSB0:rwm")},
		{"--cap-add=NET_ADMIN --cap-add=SYS_TIME", eqList("cap_add", "NET_ADMIN", "SYS_TIME")},
		{"--cap-drop=ALL", eqList("cap_drop", "ALL")},
		{"--security-opt=no-new-privileges", eqList("security_opt", "no-new-privileges")},
		{"--sysctl=net.ipv4.ip_forward=1", eq("sysctls", map[string]any{"net.ipv4.ip_forward": "1"})},
		{"--ulimit=nofile=1024:2048 --ulimit=core=0", eq("ulimits", map[string]any{"nofile": map[string]any{"soft": 1024, "hard": 2048}, "core": 0})},
		{"--dns=1.1.1.1 --dns=9.9.9.9", eqList("dns", "1.1.1.1", "9.9.9.9")},
		{"--add-host=db:10.0.0.5", eqList("extra_hosts", "db:10.0.0.5")},
		{"--tmpfs=/run:rw,size=64m", eqList("tmpfs", "/run:rw,size=64m")},
		{"--shm-size=1g", eq("shm_size", "1g")},
		{"--runtime=nvidia", eq("runtime", "nvidia")},
		{"--log-opt=max-size=10m --log-opt=max-file=3", eq("logging", map[string]any{"options": map[string]any{"max-size": "10m", "max-file": "3"}})},
		{"--stop-timeout=30", eq("stop_grace_period", "30s")},
		{"--health-cmd='curl -f http://localhost/ || exit 1' --health-interval=30s --health-timeout=5s --health-retries=3 --health-start-period=1m", eq("healthcheck", map[string]any{
			"test": []any{"CMD-SHELL", "curl -f http://localhost/ || exit 1"}, "interval": "30s", "timeout": "5s", "retries": 3, "start_period": "1m",
		})},
		{"--no-healthcheck", eq("healthcheck", map[string]any{"disable": true})},
		{"--pid=host", eq("pid", "host")},
		{"--pid=container:other", eq("pid", "container:other")},
		{"--cgroupns=host", eq("cgroup", "host")},
		{"--device-cgroup-rule='c 189:* rmw'", eqList("device_cgroup_rules", "c 189:* rmw")},
		{"-e KEY=value --env=OTHER=2", eq("environment", map[string]any{"KEY": "value", "OTHER": "2"})},
		{"-p 8080:80 -p 127.0.0.1:5353:53/udp", eqList("ports", "8080:80/tcp", "127.0.0.1:5353:53/udp")},
		{"-v /mnt/user/media:/media:ro -v data:/var/data", eqList("volumes", "/mnt/user/media:/media:ro", "data:/var/data")},
		{"--gpus all", eq("deploy", map[string]any{"resources": map[string]any{"reservations": map[string]any{"devices": []any{
			map[string]any{"driver": "nvidia", "count": "all", "capabilities": []any{"gpu"}}}}}})},
	}
	for _, tc := range cases {
		t.Run(tc.flags, func(t *testing.T) {
			c, svc := convertOK(t, "<ExtraParams>"+tc.flags+"</ExtraParams>")
			if !c.Clean() {
				t.Fatalf("a flag in the translate table left the conversion not clean: %+v", c.Warnings)
			}
			tc.check(t, svc)
		})
	}
}

func eq(key string, want any) func(*testing.T, map[string]any) {
	return func(t *testing.T, svc map[string]any) {
		t.Helper()
		if !reflect.DeepEqual(svc[key], want) {
			t.Errorf("%s = %#v, want %#v", key, svc[key], want)
		}
	}
}

func eqList(key string, want ...string) func(*testing.T, map[string]any) {
	return func(t *testing.T, svc map[string]any) {
		t.Helper()
		var got []string
		for _, e := range svc[key].([]any) {
			got = append(got, fmt.Sprint(e))
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

func TestConvertUnraid_GPUDeviceListsAndMount(t *testing.T) {
	_, svc := convertOK(t, `<ExtraParams>--gpus '"device=0,1","capabilities=compute,utility"'</ExtraParams>`)
	deploy := svc["deploy"].(map[string]any)["resources"].(map[string]any)["reservations"].(map[string]any)["devices"].([]any)
	gpu := deploy[0].(map[string]any)
	if got := strs(t, gpu["device_ids"]); !reflect.DeepEqual(got, []string{"0", "1"}) {
		t.Errorf("device_ids = %v", got)
	}

	c, svc := convertOK(t, `<ExtraParams>--mount type=bind,src=/mnt/user/media,dst=/media,readonly --mount type=volume,src=cfg,dst=/cfg --mount type=tmpfs,dst=/scratch,tmpfs-size=64m</ExtraParams>`)
	if !c.Clean() {
		t.Fatalf("--mount is in the translate table but left the conversion not clean: %+v", c.Warnings)
	}
	mounts := svc["volumes"].([]any)
	if len(mounts) != 3 {
		t.Fatalf("volumes = %v", mounts)
	}
	first := mounts[0].(map[string]any)
	if first["type"] != "bind" || first["source"] != "/mnt/user/media" || first["target"] != "/media" || first["read_only"] != true {
		t.Errorf("bind mount = %v", first)
	}
	if _, ok := yamlTop(t, c)["volumes"].(map[string]any)["cfg"]; !ok {
		t.Error("a named volume used by --mount is not declared at the top level")
	}
	if got := mounts[2].(map[string]any)["tmpfs"].(map[string]any)["size"]; got != "64m" {
		t.Errorf("tmpfs size = %v", got)
	}
}

func TestConvertUnraid_MountFieldsOutsideTheTableAreUntranslated(t *testing.T) {
	for _, m := range []string{
		"type=bind,src=/mnt/user/a,dst=/a,bind-nonrecursive=true",
		"type=volume,src=v,dst=/v,volume-driver=nfs",
		"type=npipe,src=a,dst=/a",
		"type=bind,src=relative,dst=/a",
		"type=bind,src=/mnt/user/a,dst=relative",
		"type=bind,src=/mnt/user/a,dst=/a,tmpfs-size=1m",
	} {
		c, svc := convertOK(t, "<ExtraParams>--mount "+m+"</ExtraParams>")
		if len(warningsOf(c, WarnUntranslatedFlag)) != 1 {
			t.Errorf("--mount %s: warnings = %+v", m, c.Warnings)
		}
		if _, ok := svc["volumes"]; ok {
			t.Errorf("--mount %s was partly translated: %v", m, svc["volumes"])
		}
	}
}

func TestConvertUnraid_PrivilegesComeFromTheGeneratedCompose(t *testing.T) {
	cases := []struct {
		name string
		body string
		kind string
	}{
		{"privileged element", `<Privileged>true</Privileged>`, PrivilegePrivileged},
		{"host network", `<Network>host</Network>`, PrivilegeHostNetwork},
		{"cap-add", `<ExtraParams>--cap-add=SYS_ADMIN</ExtraParams>`, PrivilegeAddedCaps},
		{"pid host", `<ExtraParams>--pid=host</ExtraParams>`, PrivilegeHostPID},
		{"pid container", `<ExtraParams>--pid=container:other</ExtraParams>`, PrivilegeHostPID},
		{"cgroupns host", `<ExtraParams>--cgroupns=host</ExtraParams>`, PrivilegeHostCgroup},
		{"device cgroup rule", `<ExtraParams>--device-cgroup-rule='c 189:* rmw'</ExtraParams>`, PrivilegeDeviceCgroupRule},
		{"group-add", `<ExtraParams>--group-add=video</ExtraParams>`, PrivilegeGroupAdd},
		{"unconfined", `<ExtraParams>--security-opt=apparmor=unconfined</ExtraParams>`, PrivilegeNoConfinement},
		{"docker socket", `<Config Type="Path" Target="/var/run/docker.sock" Mode="rw">/var/run/docker.sock</Config>`, PrivilegeDockerSocket},
		{"docker socket by flag", `<ExtraParams>-v /var/run/docker.sock:/var/run/docker.sock</ExtraParams>`, PrivilegeDockerSocket},
		{"host path by mount flag", `<ExtraParams>--mount type=bind,src=/etc,dst=/host-etc</ExtraParams>`, PrivilegeHostPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := convertOK(t, tc.body)
			found := false
			for _, p := range c.Privileges {
				found = found || p.Kind == tc.kind
			}
			if !found {
				t.Errorf("privileges = %+v, want one of kind %s", c.Privileges, tc.kind)
			}
		})
	}
	c, _ := convertOK(t, `<Config Type="Path" Target="/data" Mode="rw">/mnt/user/media</Config>`)
	if len(c.Privileges) != 0 {
		t.Errorf("an ordinary container reports privileges: %+v", c.Privileges)
	}
}

func TestConvertUnraid_HostNetworkPublishesNoPorts(t *testing.T) {
	c, svc := convertOK(t, `<Network>host</Network>
<Config Type="Port" Target="80" Mode="tcp">8080</Config>
<ExtraParams>-p 9090:90</ExtraParams>`)
	if _, ok := svc["ports"]; ok {
		t.Errorf("ports = %v with network_mode host", svc["ports"])
	}
	if got := len(warningsOf(c, WarnNote)); got < 2 {
		t.Errorf("the ignored ports are not noted: %+v", c.Warnings)
	}
	if !c.Clean() {
		t.Errorf("ignored ports under host networking make the conversion not clean: %+v", c.Warnings)
	}
}

func TestConvertUnraid_MergeWithConfigEntries(t *testing.T) {
	t.Run("exact duplicate is dropped with a note", func(t *testing.T) {
		c, svc := convertOK(t, `<Config Type="Path" Target="/media" Mode="ro">/mnt/user/media</Config>
<Config Type="Port" Target="80" Mode="tcp">8080</Config>
<Config Type="Variable" Target="TZ">UTC</Config>
<ExtraParams>-v /mnt/user/media:/media:ro -p 8080:80 -e TZ=UTC</ExtraParams>`)
		if got := strs(t, svc["volumes"]); len(got) != 1 {
			t.Errorf("volumes = %v", got)
		}
		if got := strs(t, svc["ports"]); len(got) != 1 {
			t.Errorf("ports = %v", got)
		}
		if len(warningsOf(c, WarnConflict)) != 0 || !c.Clean() {
			t.Errorf("an exact duplicate is a conflict: %+v", c.Warnings)
		}
		if got := len(warningsOf(c, WarnNote)); got < 3 {
			t.Errorf("dropped duplicates are not each noted: %+v", c.Warnings)
		}
	})
	conflicts := []struct{ name, body, kept, left string }{
		{"access mode", `<Config Type="Path" Target="/media" Mode="rw">/mnt/user/media</Config><ExtraParams>-v /mnt/user/media:/media:ro</ExtraParams>`, "/mnt/user/media:/media", "ro"},
		{"host path", `<Config Type="Path" Target="/media" Mode="rw">/mnt/user/media</Config><ExtraParams>-v /mnt/cache/other:/media</ExtraParams>`, "/mnt/user/media:/media", "/mnt/cache/other:/media"},
		{"bind address", `<Config Type="Port" Target="80" Mode="tcp">8080</Config><ExtraParams>-p 127.0.0.1:8080:80</ExtraParams>`, "8080:80/tcp", "127.0.0.1:8080:80/tcp"},
		{"host port", `<Config Type="Port" Target="80" Mode="tcp">8080</Config><ExtraParams>-p 9090:80</ExtraParams>`, "8080:80/tcp", "9090:80/tcp"},
		{"variable value", `<Config Type="Variable" Target="TZ">UTC</Config><ExtraParams>-e TZ=Europe/Vienna</ExtraParams>`, "TZ=UTC", "TZ=Europe/Vienna"},
		{"two extra params", `<ExtraParams>-e A=1 -e A=2</ExtraParams>`, "A=1", "A=2"},
	}
	for _, tc := range conflicts {
		t.Run("conflict on "+tc.name, func(t *testing.T) {
			c, svc := convertOK(t, tc.body)
			ws := warningsOf(c, WarnConflict)
			if len(ws) != 1 {
				t.Fatalf("conflict warnings = %+v", c.Warnings)
			}
			if !strings.Contains(ws[0].Message, tc.kept) || !strings.Contains(ws[0].Message, tc.left) {
				t.Errorf("the warning does not list both entries (%q and %q): %s", tc.kept, tc.left, ws[0].Message)
			}
			if c.Clean() {
				t.Error("a conflict leaves the conversion clean")
			}
			for _, k := range []string{"volumes", "ports", "environment"} {
				if v, ok := svc[k]; ok && strings.Contains(fmt.Sprint(v), strings.TrimPrefix(tc.left, "TZ=")) && tc.name != "access mode" && tc.name != "two extra params" {
					t.Errorf("the left-out entry %q is in the generated %s: %v", tc.left, k, v)
				}
			}
		})
	}
}

func TestConvertUnraid_CustomNetworkReportsTheCreateCommand(t *testing.T) {
	body := `<Network>br0</Network><MyIP>192.168.1.50</MyIP>`
	without, err := ConvertUnraid(unraidXML(body), ConvertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ws := warningsOf(without, WarnMissingNetwork)
	if len(ws) != 1 {
		t.Fatalf("missing network warnings = %+v", without.Warnings)
	}
	wantPlaceholder := "docker network create -d macvlan --subnet <SUBNET> -o parent=<INTERFACE> br0"
	if ws[0].Command != wantPlaceholder {
		t.Errorf("command without a definition = %q, want %q", ws[0].Command, wantPlaceholder)
	}
	if !strings.Contains(ws[0].Message, "placeholder") && !strings.Contains(ws[0].Message, "PLACEHOLDER") {
		t.Errorf("the warning does not say the values were not known: %s", ws[0].Message)
	}
	if without.Clean() {
		t.Error("a missing network leaves the conversion clean")
	}

	with, err := ConvertUnraid(unraidXML(body), ConvertOptions{Networks: []NetworkDef{{
		Name: "br0", Driver: "macvlan", Subnet: "192.168.1.0/24", Gateway: "192.168.1.1", IPRange: "192.168.1.128/25", Parent: "eth0",
		Options: map[string]string{"macvlan_mode": "bridge"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	ws = warningsOf(with, WarnMissingNetwork)
	if len(ws) != 1 {
		t.Fatalf("missing network warnings = %+v", with.Warnings)
	}
	wantFull := "docker network create -d macvlan --subnet 192.168.1.0/24 --gateway 192.168.1.1 --ip-range 192.168.1.128/25 -o parent=eth0 -o macvlan_mode=bridge br0"
	if ws[0].Command != wantFull {
		t.Errorf("command with a definition = %q, want %q", ws[0].Command, wantFull)
	}
	if strings.Contains(ws[0].Message, "PLACEHOLDER") || strings.Contains(ws[0].Command, "<") {
		t.Errorf("a complete command still has placeholders: %+v", ws[0])
	}

	svc := serviceOf(t, with)
	nets := svc["networks"].(map[string]any)["br0"].(map[string]any)
	if nets["ipv4_address"] != "192.168.1.50" {
		t.Errorf("networks = %v", svc["networks"])
	}
	if ext := yamlTop(t, with)["networks"].(map[string]any)["br0"].(map[string]any)["external"]; ext != true {
		t.Errorf("br0 is not declared external: %v", ext)
	}
	if _, ok := svc["network_mode"]; ok {
		t.Errorf("a custom network also set network_mode: %v", svc["network_mode"])
	}
}

func TestConvertUnraid_NetworkCommandQuotesWhatItDoesNotKnowToBeSafe(t *testing.T) {
	cmd, complete := networkCreateCommand("lan; rm -rf /", &NetworkDef{Driver: "macvlan", Subnet: "10.0.0.0/24", Parent: "eth0"})
	if !complete {
		t.Error("a full definition is not complete")
	}
	if !strings.HasSuffix(cmd, " 'lan; rm -rf /'") {
		t.Errorf("the network name is not quoted: %s", cmd)
	}
}

func TestConvertUnraid_BridgeHostAndNoneMapToNetworkMode(t *testing.T) {
	for _, mode := range []string{"bridge", "host", "none"} {
		c, svc := convertOK(t, "<Network>"+mode+"</Network>")
		if svc["network_mode"] != mode {
			t.Errorf("%s: network_mode = %v", mode, svc["network_mode"])
		}
		if len(warningsOf(c, WarnMissingNetwork)) != 0 {
			t.Errorf("%s is reported as a missing network", mode)
		}
	}
}

func TestConvertUnraid_WritableLayerWarningIsAlwaysItsOwnClass(t *testing.T) {
	for _, body := range []string{``, `<ExtraParams>--whatever</ExtraParams>`, `<Network>host</Network>`} {
		c, _ := convertOK(t, body)
		ws := warningsOf(c, WarnWritableLayer)
		if len(ws) != 1 {
			t.Fatalf("%q: writable layer warnings = %+v", body, ws)
		}
		for _, want := range []string{"docker exec", "mapped volume", "recreating the container"} {
			if !strings.Contains(ws[0].Message, want) {
				t.Errorf("writable-layer warning lacks %q: %s", want, ws[0].Message)
			}
		}
	}
	c, _ := convertOK(t, ``)
	if !c.Clean() {
		t.Errorf("the writable-layer warning makes a clean conversion not clean: %+v", c.Warnings)
	}
}

func TestConvertUnraid_LatestAndUnpinnedTagsAreNotes(t *testing.T) {
	for _, repo := range []string{"lscr.io/linuxserver/foo", "lscr.io/linuxserver/foo:latest", "localhost:5000/foo"} {
		c, err := ConvertUnraid([]byte(`<Container><Name>a</Name><Repository>`+repo+`</Repository></Container>`), ConvertOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !hasWarning(c, WarnNote, "latest") || !c.Clean() {
			t.Errorf("%s: warnings = %+v, want a latest note and a clean conversion", repo, c.Warnings)
		}
	}
	c, _ := convertOK(t, ``)
	if hasWarning(c, WarnNote, "latest") {
		t.Errorf("a pinned tag is flagged: %+v", c.Warnings)
	}
}

func TestConvertUnraid_ValuesAreLiteralInCompose(t *testing.T) {
	c, svc := convertOK(t, `<Config Type="Variable" Target="PASSWORD">pa$word${X}</Config>
<PostArgs>--arg '$HOME'</PostArgs>`)
	if !strings.Contains(c.Compose, "pa$$word$${X}") {
		t.Errorf("a $ in a value is not escaped for Compose:\n%s", c.Compose)
	}
	if got := strs(t, svc["command"]); got[1] != "$$HOME" {
		t.Errorf("command = %v, want the $ escaped", got)
	}
	if got := interpolate("pa$$word$${X}", nil); got != "pa$word${X}" {
		t.Errorf("Compose reads the escaped value as %q", got)
	}
}

func TestConvertUnraid_TemplateTextCannotInjectComposeKeys(t *testing.T) {
	// A newline inside a quoted ExtraParams word must not end the comment
	// the word is shown in and start a YAML key of its own.
	c, svc := convertOK(t, "<ExtraParams>--evil='a&#10;    privileged: true&#10;    user: root'</ExtraParams>")
	if _, ok := svc["privileged"]; ok {
		t.Errorf("template text became a Compose key:\n%s", c.Compose)
	}
	if _, ok := svc["user"]; ok {
		t.Errorf("template text became a Compose key:\n%s", c.Compose)
	}
	for _, line := range strings.Split(c.Compose, "\n") {
		if strings.Contains(line, "privileged: true") && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Errorf("injected line outside a comment: %q", line)
		}
	}
	if len(c.Privileges) != 0 {
		t.Errorf("injected text reached the privilege summary: %+v", c.Privileges)
	}
}

func TestConvertUnraid_ServiceNamesAreValidComposeKeys(t *testing.T) {
	for name, want := range map[string]string{
		"plex":              "plex",
		"Plex Media Server": "Plex-Media-Server",
		"../etc/x":          "etc-x",
		"@@@":               "app",
	} {
		c, err := ConvertUnraid([]byte(`<Container><Name>`+name+`</Name><Repository>x/y:1</Repository></Container>`), ConvertOptions{})
		if err != nil {
			t.Fatal(err)
		}
		svc := serviceOf(t, c)
		if svc["container_name"] != want {
			t.Errorf("%q: container_name = %v, want %s", name, svc["container_name"], want)
		}
		var doc struct {
			Services map[string]any `yaml:"services"`
		}
		if err := yaml.Unmarshal([]byte(c.Compose), &doc); err != nil {
			t.Fatal(err)
		}
		if _, ok := doc.Services[want]; !ok {
			t.Errorf("%q: service key is not %s: %v", name, want, doc.Services)
		}
	}
}

func TestConvertUnraid_RepeatedScalarFlagUsesTheLastValueAndSaysSo(t *testing.T) {
	c, svc := convertOK(t, `<ExtraParams>--user=1 --user=2</ExtraParams>`)
	if svc["user"] != "2" {
		t.Errorf("user = %v, want the last value", svc["user"])
	}
	if !hasWarning(c, WarnNote, "more than once") {
		t.Errorf("a repeated flag is not noted: %+v", c.Warnings)
	}
}

func TestConvertUnraid_RepeatedListFlagKeepsEveryValueWithoutANote(t *testing.T) {
	c, svc := convertOK(t, `<ExtraParams>--cap-add=NET_ADMIN --cap-add=SYS_TIME --security-opt=no-new-privileges --security-opt=apparmor=unconfined</ExtraParams>`)
	if caps, _ := svc["cap_add"].([]any); len(caps) != 2 {
		t.Errorf("cap_add = %v, want both capabilities", svc["cap_add"])
	}
	if opts, _ := svc["security_opt"].([]any); len(opts) != 2 {
		t.Errorf("security_opt = %v, want both options", svc["security_opt"])
	}
	if hasWarning(c, WarnNote, "more than once") {
		t.Errorf("a list flag is noted as if a value were dropped: %+v", c.Warnings)
	}
}

func TestConvertUnraid_SecondElementWithAnotherValueIsReported(t *testing.T) {
	c, svc := convertOK(t, `<Network>host</Network><Network>bridge</Network>`)
	if svc["network_mode"] != "host" {
		t.Errorf("network_mode = %v", svc["network_mode"])
	}
	if !hasWarning(c, WarnUntranslatedField, "Network") {
		t.Errorf("the second <Network> is dropped silently: %+v", c.Warnings)
	}
}

func TestConvertUnraid_ReturnsTheSourceAndLeavesItUntouched(t *testing.T) {
	c, err := ConvertUnraid([]byte(fullTemplate), ConvertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != fullTemplate {
		t.Error("Source is not the template as it was given")
	}
}

func TestConvertUnraid_RefusesInputThatIsNotATemplate(t *testing.T) {
	cases := map[string]string{
		"empty":             ``,
		"not xml":           `hello`,
		"truncated":         `<Container><Name>a</Name>`,
		"wrong root":        `<Compose><Repository>x</Repository></Compose>`,
		"no repository":     `<Container><Name>a</Name></Container>`,
		"empty repository":  `<Container><Repository>  </Repository></Container>`,
		"bad repository":    `<Container><Repository>x y</Repository></Container>`,
		"repository option": `<Container><Repository>--privileged</Repository></Container>`,
		"entity":            `<!DOCTYPE c [<!ENTITY a "b">]><Container><Repository>&a;</Repository></Container>`,
		"binary":            "\x00\x01\x02",
		"oversized":         `<Container><Repository>x</Repository><Overview>` + strings.Repeat("a", MaxUnraidTemplateBytes) + `</Overview></Container>`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ConvertUnraid([]byte(in), ConvertOptions{})
			if !errors.Is(err, ErrInvalidUnraidTemplate) {
				t.Errorf("err = %v, want ErrInvalidUnraidTemplate", err)
			}
		})
	}
}

// The converter reads third-party text: no input may panic it.
func TestConvertUnraid_NeverPanicsOnOddFlags(t *testing.T) {
	odd := []string{
		"", "-", "--", "---", "-=", "--=", "-m", "-mm", "-=x", "--memory=", "-p", "-p ::", "-p [::1", "-p [::1]:80", "-p ]:",
		"-v", "-v ::::", "-v :", "-v /:", "--mount", "--mount ,", "--mount =", "--mount type", "--mount \"", "--gpus", "--gpus ,", "--gpus =",
		"--ulimit", "--ulimit =", "--ulimit a=", "--add-host :", "--add-host x:", "-e =", "-e ==", "--device :", "--entrypoint",
		"\\", "'", "\"", "-itd -itd -it", strings.Repeat("-m1 ", 200), "--health-cmd=", "--cpus=1e5", "--stop-timeout=-1",
	}
	for _, flags := range odd {
		body := "<ExtraParams>" + strings.NewReplacer("&", "&amp;", "<", "&lt;").Replace(flags) + "</ExtraParams>"
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ExtraParams %q panicked: %v", flags, r)
				}
			}()
			c, err := ConvertUnraid(unraidXML(body), ConvertOptions{})
			if err != nil {
				t.Errorf("ExtraParams %q: %v", flags, err)
				return
			}
			serviceOf(t, c)
		}()
	}
}

func TestConvertUnraid_EveryWarningClassIsKnown(t *testing.T) {
	c, _ := convertOK(t, `<ExtraParams>--x -v /boot:/b -e A=1 -e A=2</ExtraParams><Network>br0</Network><Gizmo>1</Gizmo>`)
	known := map[string]bool{WarnUntranslatedFlag: true, WarnUntranslatedField: true, WarnFlaggedPath: true, WarnMissingNetwork: true, WarnConflict: true, WarnWritableLayer: true, WarnNote: true}
	var classes []string
	for _, w := range c.Warnings {
		if !known[w.Class] {
			t.Errorf("unknown warning class %q", w.Class)
		}
		classes = append(classes, w.Class)
	}
	sort.Strings(classes)
	for _, class := range []string{WarnUntranslatedFlag, WarnUntranslatedField, WarnFlaggedPath, WarnMissingNetwork, WarnConflict, WarnWritableLayer} {
		if !hasClass(classes, class) {
			t.Errorf("no %s warning in %v", class, classes)
		}
	}
}

func hasClass(classes []string, class string) bool {
	for _, c := range classes {
		if c == class {
			return true
		}
	}
	return false
}

func TestConvertUnraid_UnraidSpecificVariablesAreNoted(t *testing.T) {
	c, svc := convertOK(t, `<Config Type="Variable" Target="HOST_OS">Unraid</Config>
<Config Type="Variable" Target="TOKEN">a$$b</Config>`)
	if !hasWarning(c, WarnNote, "HOST_OS") || !hasWarning(c, WarnNote, "$$") {
		t.Errorf("HOST_OS and $$ values are not noted: %+v", c.Warnings)
	}
	env := svc["environment"].(map[string]any)
	if env["HOST_OS"] != "Unraid" || interpolate(env["TOKEN"].(string), nil) != "a$$b" {
		t.Errorf("environment = %v, want both carried (Compose must read TOKEN as the literal a$$b)", env)
	}
	if !c.Clean() {
		t.Errorf("notes make the conversion not clean: %+v", c.Warnings)
	}
}

func TestConvertUnraid_SharedContainerNetwork(t *testing.T) {
	c, svc := convertOK(t, `<Network>container:vpn</Network><Config Type="Port" Target="80" Mode="tcp">8080</Config>`)
	if svc["network_mode"] != "container:vpn" {
		t.Errorf("network_mode = %v", svc["network_mode"])
	}
	if _, ok := svc["ports"]; ok {
		t.Errorf("ports published on a shared network: %v", svc["ports"])
	}
	if !hasWarning(c, WarnNote, "vpn") {
		t.Errorf("the dependency on the other container is not noted: %+v", c.Warnings)
	}
}

func TestConvertUnraid_AnUnusableVariableOrDeviceIsReportedNotDropped(t *testing.T) {
	c, _ := convertOK(t, `<Config Type="Variable" Target="">x</Config>`)
	if !hasWarning(c, WarnUntranslatedField, "x") || c.Clean() {
		t.Errorf("a nameless variable is dropped silently: %+v", c.Warnings)
	}
	c, svc := convertOK(t, `<Config Type="Device" Target="/dev/ttyUSB1" Mode="">/dev/ttyUSB0</Config>`)
	if !hasWarning(c, WarnNote, "/dev/ttyUSB1") {
		t.Errorf("a device container path Unraid ignores is not noted: %+v", c.Warnings)
	}
	if got := strs(t, svc["devices"]); !reflect.DeepEqual(got, []string{"/dev/ttyUSB0:/dev/ttyUSB0"}) {
		t.Errorf("devices = %v", got)
	}
}

func TestConvertUnraid_VersionOneBlocksAreReportedNotDropped(t *testing.T) {
	src := []byte(`<?xml version="1.0"?>
<Container>
  <Name>sonarr</Name>
  <Repository>lscr.io/linuxserver/sonarr:4.0.0</Repository>
  <Networking>
    <Mode>host</Mode>
    <Publish><Port><HostPort>8989</HostPort><ContainerPort>8989</ContainerPort><Protocol>tcp</Protocol></Port></Publish>
  </Networking>
  <Data>
    <Volume><HostDir>/mnt/user/appdata/sonarr</HostDir><ContainerDir>/config</ContainerDir><Mode>rw</Mode></Volume>
    <Volume><HostDir>/boot/config</HostDir><ContainerDir>/boot</ContainerDir><Mode>ro</Mode></Volume>
  </Data>
  <Environment><Variable><Name>PUID</Name><Value>99</Value></Variable></Environment>
</Container>`)
	c, err := ConvertUnraid(src, ConvertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Clean() {
		t.Fatalf("a template whose ports, paths and variables were not read is reported clean: %+v", c.Warnings)
	}
	for _, block := range []string{"Networking", "Data", "Environment"} {
		found := false
		for _, w := range warningsOf(c, WarnUntranslatedField) {
			found = found || strings.Contains(w.Message, "<"+block+">")
		}
		if !found {
			t.Errorf("no untranslated_field warning for <%s>: %+v", block, c.Warnings)
		}
	}
}

func TestConvertUnraid_AKnownFieldWithNestedElementsIsReported(t *testing.T) {
	for _, body := range []string{
		`<Network><Mode>host</Mode></Network>`,
		`<Config Type="Path" Target="/data" Mode="rw"><Gizmo/>/mnt/user/x</Config>`,
	} {
		c, svc := convertOK(t, body)
		if c.Clean() || len(warningsOf(c, WarnUntranslatedField)) == 0 {
			t.Errorf("%s: dropped without a warning: %+v", body, c.Warnings)
		}
		if _, ok := svc["volumes"]; ok {
			t.Errorf("%s: a part that was not read became a mount: %v", body, svc["volumes"])
		}
	}
}

func TestConvertUnraid_MarkupInDisplayTextIsNotAnUntranslatedField(t *testing.T) {
	c, _ := convertOK(t, `<Overview>Does <b>things</b> well.</Overview>`)
	if !c.Clean() {
		t.Errorf("markup in an overview makes the conversion unclean: %+v", c.Warnings)
	}
}

func TestConvertUnraid_AColonInAConfigPathCannotChangeTheMount(t *testing.T) {
	cases := []struct{ name, body string }{
		{"colon in the host path", `<Config Type="Path" Target="/data" Mode="rw">/mnt/user/../../etc:/a/../../../mnt/user/x</Config>`},
		{"colon in the container path", `<Config Type="Path" Target="/config:ro" Mode="rw">/mnt/user/appdata/x</Config>`},
		{"option smuggled through the host path", `<Config Type="Path" Target="/data" Mode="rw">/mnt/user/x:/data:ro</Config>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, svc := convertOK(t, tc.body)
			if c.Clean() || !hasWarning(c, WarnUntranslatedField, "colon") {
				t.Errorf("no untranslated_field warning about the colon: %+v", c.Warnings)
			}
			if v, ok := svc["volumes"]; ok {
				t.Errorf("a path with a colon was written as a mount: %v", v)
			}
		})
	}
}

func TestConvertUnraid_EveryShortMountIsOneSourceAndOneTarget(t *testing.T) {
	for _, body := range []string{
		`<Config Type="Path" Target="/data" Mode="rw">/mnt/user/media</Config>`,
		`<ExtraParams>-v /mnt/user/media:/data:ro</ExtraParams>`,
		`<ExtraParams>-v /mnt/user/a:/a:b:c</ExtraParams>`,
		`<ExtraParams>--mount type=bind,src=/mnt/user/a:/etc,dst=/data</ExtraParams>`,
	} {
		_, svc := convertOK(t, body)
		list, _ := svc["volumes"].([]any)
		for _, v := range list {
			if s, ok := v.(string); ok && strings.Count(s, ":") > 2 {
				t.Errorf("%s: %q holds more than source:target:options", body, s)
			}
		}
	}
}
