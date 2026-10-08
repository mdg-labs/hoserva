package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const probeBlock = "x-hoserva:\n  schema: 1\n  id: probe\n  revision: 1\n  title: Probe\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n"

func checkCompose(t *testing.T, body string) []Issue {
	t.Helper()
	tpl, issues := Parse([]byte(body + probeBlock))
	if tpl == nil {
		t.Fatalf("parse: %v", issues)
	}
	return tpl.Check()
}

func TestEveryFixtureTemplatePassesCheck(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(fixtureDir, "*", ComposeFile))
	if err != nil || len(files) < 4 {
		t.Fatalf("fixture templates = %v, %v", files, err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		tpl, issues := Parse(data)
		if tpl == nil {
			t.Errorf("%s: %v", f, issues)
			continue
		}
		if issues := tpl.Check(); len(issues) > 0 {
			t.Errorf("%s: %v", f, issues)
		}
	}
}

// converterKeys are every Compose field the Unraid converter emits (doc 04
// §5), plus the deploy form of a GPU reservation.
func TestTheKeysTheConverterEmitsPassCheck(t *testing.T) {
	const compose = `services:
  app:
    image: registry.example.com/app:1.0
    container_name: app
    command: ["serve"]
    entrypoint: ["/start"]
    environment: { A: b }
    labels: { a: b }
    restart: unless-stopped
    mem_limit: 512m
    memswap_limit: 1g
    cpus: 2
    cpuset: "0-1"
    pids_limit: 100
    user: "99:100"
    working_dir: /app
    hostname: app
    group_add: ["44"]
    stdin_open: true
    tty: true
    init: true
    read_only: true
    devices: ["/dev/dri:/dev/dri"]
    cap_add: [NET_ADMIN]
    cap_drop: [MKNOD]
    security_opt: ["no-new-privileges:true"]
    sysctls: { net.ipv4.ip_forward: 1 }
    ulimits: { nofile: 1024 }
    dns: [1.1.1.1]
    extra_hosts: ["a:10.0.0.1"]
    tmpfs: [/tmp]
    shm_size: 1g
    runtime: nvidia
    logging: { driver: json-file, options: { max-size: 10m } }
    stop_grace_period: 30s
    healthcheck: { test: ["CMD-SHELL", "true"], interval: 30s, retries: 3 }
    ports: ["8080:80/tcp"]
    networks: [lan]
    network_mode: bridge
    volumes:
      - /mnt/user/media:/media:ro
      - data:/data
      - { type: bind, source: /mnt/cache/appdata/app, target: /config }
      - { type: tmpfs, target: /scratch }
    deploy:
      resources:
        limits: { memory: 1g }
        reservations:
          devices: [{ driver: nvidia, count: 1, capabilities: [gpu] }]
    x-note: ignored
volumes:
  data: {}
  share:
    driver: local
    driver_opts: { type: nfs, o: "addr=10.0.0.2", device: ":/export" }
networks:
  lan: { external: true }
x-shared: &shared
  a: b
`
	if issues := checkCompose(t, compose); len(issues) > 0 {
		t.Errorf("issues: %v", issues)
	}
	tpl, issues := Parse([]byte(compose + probeBlock))
	if tpl == nil {
		t.Fatalf("parse: %v", issues)
	}
	got := strings.Join(privilegeKinds(tpl.Privileges(nil)), "|")
	want := "added_capabilities:NET_ADMIN|group_add:44|container_runtime:nvidia|gpu_reservation:driver nvidia, count 1, capabilities gpu|host_path:/dev/dri"
	if got != want {
		t.Errorf("privileges = %q, want %q", got, want)
	}
}

func TestCheckAcceptsOnlyComposeKeysTheSummaryClassifies(t *testing.T) {
	tests := []struct{ name, compose, want string }{
		{"top-level secrets", "services:\n  a: { image: x }\nsecrets: {}\n", "secrets and configs mount"},
		{"unknown top-level key of any name", "services:\n  a: { image: x }\nfuture_feature: {}\n", `"future_feature" is not accepted`},
		{"unknown service key", "services:\n  a: { image: x, future_feature: true }\n", `"future_feature" is not accepted`},
		{"service secrets", "services:\n  a: { image: x, secrets: [s] }\n", "secrets and configs mount"},
		{"service configs", "services:\n  a: { image: x, configs: [c] }\n", "secrets and configs mount"},
		{"build", "services:\n  a: { build: /etc }\n", "build reads a host directory"},
		{"build with additional contexts", "services:\n  a: { image: x, build: { context: ., additional_contexts: { host: /root } } }\n", "build reads a host directory"},
		{"host user namespace", "services:\n  a: { image: x, userns_mode: host }\n", `"userns_mode" is not accepted`},
		{"cgroup parent", "services:\n  a: { image: x, cgroup_parent: /system.slice }\n", `"cgroup_parent" is not accepted`},
		{"lifecycle hook", "services:\n  a: { image: x, post_start: [{ command: id }] }\n", `"post_start" is not accepted`},
		{"host ipc", "services:\n  a: { image: x, ipc: host }\n", "must be private, shareable, none"},
		{"ipc of an outside container", "services:\n  a: { image: x, ipc: \"container:b\" }\n", "must be private, shareable, none"},
		{"ipc of a service of the template", "services:\n  a: { image: x, ipc: \"service:b\" }\n  b: { image: x }\n", ""},
		{"interpolated pull policy", "services:\n  a: { image: x, pull_policy: \"${P}\" }\n", "must be spelled out"},
		{"network of another container", "services:\n  a: { image: x, network_mode: \"container:b\" }\n", "may share the network only of a service"},
		{"network of a service of the template", "services:\n  a: { image: x, network_mode: \"service:b\" }\n  b: { image: x }\n", ""},
		{"interpolated network mode", "services:\n  a: { image: x, network_mode: \"${M}\" }\n", "must be spelled out"},
		{"host network by name", "services:\n  a: { image: x, networks: [host] }\n", "attaches to the host network"},
		{"host network by external name", "services:\n  a: { image: x, networks: [h] }\nnetworks:\n  h: { external: true, name: host }\n", "must not be host"},
		{"host network driver", "services:\n  a: { image: x, networks: [h] }\nnetworks:\n  h: { driver: host }\n", "must not be host"},
		{"host network by the older external mapping", "services:\n  a: { image: x, networks: [h] }\nnetworks:\n  h: { external: { name: host } }\n", "external: must be true or false"},
		{"external network as a word", "services:\n  a: { image: x, networks: [h] }\nnetworks:\n  h: { external: \"true\" }\n", "external: must be true or false"},
		{"external network as true", "services:\n  a: { image: x, networks: [h] }\nnetworks:\n  h: { external: true }\n", ""},
		{"external volume of the older mapping", "services:\n  a: { image: x }\nvolumes:\n  v: { external: { name: other } }\n", `"external" is not accepted`},
		{"privileged an unknown word", "services:\n  a: { image: x, privileged: maybe }\n", "privileged: must be true or false"},
		{"privileged as a number", "services:\n  a: { image: x, privileged: 1 }\n", "privileged: must be true or false"},
		{"privileged y", "services:\n  a: { image: x, privileged: y }\n", ""},
		{"unknown network key", "services:\n  a: { image: x }\nnetworks:\n  h: { future: 1 }\n", `"future" is not accepted`},
		{"env file with another path", "services:\n  a: { image: x, env_file: [\"${HOME}/.env\"] }\n", "only the stack's own .env"},
		{"mount of an image", "services:\n  a: { image: x, volumes: [{ type: image, source: y, target: /y }] }\n", "bind, volume or tmpfs"},
		{"mount type from an input", "services:\n  a: { image: x, volumes: [{ type: \"${T}\", source: /etc, target: /y }] }\n", "bind, volume or tmpfs"},
		{"non-mapping service", "services:\n  a: { image: x }\n  b: nope\n", "must be a mapping"},
		{"deploy key", "services:\n  a: { image: x, deploy: { placement: {} } }\n", `"placement" is not accepted`},
		{"deploy resource key", "services:\n  a: { image: x, deploy: { resources: { future: 1 } } }\n", `"future" is not accepted`},
		{"external volume", "services:\n  a: { image: x }\nvolumes:\n  v: { external: true }\n", `"external" is not accepted`},
		{"volume named after another volume", "services:\n  a: { image: x }\nvolumes:\n  v: { name: other_data }\n", `"name" is not accepted`},
		{"volume of another driver", "services:\n  a: { image: x }\nvolumes:\n  v: { driver: rclone }\n", "must be local"},
		{"volume of the local driver", "services:\n  a: { image: x }\nvolumes:\n  v: { driver: local }\n", ""},
		{"volume of an overlay type", "services:\n  a: { image: x }\nvolumes:\n  v: { driver_opts: { type: overlay, o: \"lowerdir=/etc\", device: overlay } }\n", "another file system type"},
		{"volume type from an input", "services:\n  a: { image: x }\nvolumes:\n  v: { driver_opts: { type: \"${T}\", device: /x } }\n", "another file system type"},
		{"volume with an unknown option key", "services:\n  a: { image: x }\nvolumes:\n  v: { driver_opts: { type: nfs, extra: x } }\n", "only the string options"},
		{"volume with a bind and no type", "services:\n  a: { image: x }\nvolumes:\n  v: { driver_opts: { o: bind, device: /etc } }\n", ""},
	}
	for _, tc := range tests {
		issues := checkCompose(t, tc.compose)
		var msgs []string
		for _, is := range issues {
			msgs = append(msgs, is.String())
		}
		got := strings.Join(msgs, "; ")
		switch {
		case tc.want == "" && got != "":
			t.Errorf("%s: refused: %s", tc.name, got)
		case tc.want != "" && !strings.Contains(got, tc.want):
			t.Errorf("%s: issues %q, want one with %q", tc.name, got, tc.want)
		}
	}
}

// CheckCompose is the allow list and the self-contained check over a bare
// Compose document, and a template's Check reports the same issues for it.
func TestCheckComposeHoldsAnyComposeDocumentToTheAllowList(t *testing.T) {
	cases := map[string]string{
		"services:\n  web:\n    image: x\n    secrets: [a]\nsecrets:\n  a:\n    file: /f\n": "secrets",
		"services:\n  web:\n    image: x\n    volumes_from: [\"container:c\"]\n":            "volumes_from",
		"services:\n  web:\n    extends:\n      file: o.yaml\n      service: web\n":         "extends",
		"include: [o.yaml]\nservices:\n  web:\n    image: x\n":                              "include",
		"services:\n  web:\n    image: x\n    gpus: all\n":                                  "gpus",
	}
	for body, want := range cases {
		var compose map[string]any
		if err := yaml.Unmarshal([]byte(body), &compose); err != nil {
			t.Fatal(err)
		}
		issues := CheckCompose(compose)
		if len(issues) == 0 || !strings.Contains(issues[0].String(), want) {
			t.Errorf("CheckCompose(%q) = %v, want an issue on %s", body, issues, want)
		}
		var viaCheck []Issue
		for _, i := range checkCompose(t, body) {
			if strings.Contains(i.String(), want) {
				viaCheck = append(viaCheck, i)
			}
		}
		if len(viaCheck) == 0 {
			t.Errorf("a template with %q is not refused for %s by Check", body, want)
		}
	}
	var ok map[string]any
	if err := yaml.Unmarshal([]byte("services:\n  db:\n    image: x\n  web:\n    image: x\n    volumes_from: [db]\n"), &ok); err != nil {
		t.Fatal(err)
	}
	if issues := CheckCompose(ok); len(issues) != 0 {
		t.Errorf("CheckCompose(own volumes_from) = %v, want none", issues)
	}
}

// A template is still refused for a top-level version: only an imported
// project is exempt from the allow list for it (CheckImportedCompose).
func TestCheckRefusesATopLevelVersionInATemplate(t *testing.T) {
	var found bool
	for _, i := range checkCompose(t, "version: '3'\nservices:\n  web:\n    image: x\n") {
		found = found || strings.Contains(i.String(), `"version"`)
	}
	if !found {
		t.Error("a template with a top-level version is not refused")
	}
}
