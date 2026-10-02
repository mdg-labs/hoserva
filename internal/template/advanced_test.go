package template

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mdg-labs/hoserva/internal/container"
)

func f64(v float64) *float64 { return &v }

func num(v int) *int { return &v }

func advancedInstaller(t *testing.T) (*Installer, *fakeStacks, *container.FakeProvider) {
	t.Helper()
	in, stacks := newInstaller(t)
	engine := container.NewFakeProvider()
	engine.SetNetworks(
		container.Network{Name: "bridge", Driver: "bridge"},
		container.Network{Name: "host", Driver: "host"},
		container.Network{Name: "none", Driver: "null"},
		container.Network{Name: "lan", Driver: "macvlan"},
	)
	in.Networks = engine
	return in, stacks, engine
}

func planService(t *testing.T, compose, name string) map[string]any {
	t.Helper()
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		t.Fatal(err)
	}
	svc, ok := doc.Services[name]
	if !ok {
		t.Fatalf("no service %s in:\n%s", name, compose)
	}
	return svc
}

func previewWith(t *testing.T, in *Installer, adv Advanced) *Plan {
	t.Helper()
	plan, err := in.Preview(context.Background(), PlanRequest{ID: "jellyfin", Advanced: adv})
	if err != nil {
		t.Fatalf("Preview(%+v): %v", adv, err)
	}
	return plan
}

func TestEachAdvancedSettingChangesTheGeneratedCompose(t *testing.T) {
	in, _, _ := advancedInstaller(t)
	for _, tc := range []struct {
		name  string
		adv   Advanced
		field string
		want  any
	}{
		{"host network", Advanced{NetworkMode: "host"}, "network_mode", "host"},
		{"bridge network", Advanced{NetworkMode: "bridge"}, "network_mode", "bridge"},
		{"restart", Advanced{Restart: "always"}, "restart", "always"},
		{"memory", Advanced{MemoryMiB: num(512)}, "mem_limit", "512m"},
		{"cpus", Advanced{CPUs: f64(1.5)}, "cpus", 1.5},
		{"a whole number of cpus", Advanced{CPUs: f64(2)}, "cpus", 2},
		{"an extra parameter", Advanced{ExtraParams: "--hostname jelly"}, "hostname", "jelly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := planService(t, previewWith(t, in, tc.adv).Compose, "jellyfin")
			if got := svc[tc.field]; got != tc.want {
				t.Errorf("%s = %v (%T), want %v", tc.field, got, got, tc.want)
			}
		})
	}
	if got := planService(t, previewWith(t, in, Advanced{}).Compose, "jellyfin")["restart"]; got != "unless-stopped" {
		t.Errorf("restart without a setting = %v, want the template's", got)
	}
}

func TestAnExistingNetworkReplacesTheServiceNetworkAndIsDeclaredExternal(t *testing.T) {
	in, _, _ := advancedInstaller(t)
	plan := previewWith(t, in, Advanced{NetworkMode: "lan"})
	var doc struct {
		Services map[string]struct {
			NetworkMode string                    `yaml:"network_mode"`
			Networks    map[string]map[string]any `yaml:"networks"`
		} `yaml:"services"`
		Networks map[string]map[string]any `yaml:"networks"`
	}
	if err := yaml.Unmarshal([]byte(plan.Compose), &doc); err != nil {
		t.Fatal(err)
	}
	svc := doc.Services["jellyfin"]
	if _, ok := svc.Networks["lan"]; !ok || len(svc.Networks) != 1 || svc.NetworkMode != "" {
		t.Errorf("service network = %+v, mode %q, want only lan", svc.Networks, svc.NetworkMode)
	}
	if doc.Networks["lan"]["external"] != true {
		t.Errorf("top-level networks = %v, want lan declared external", doc.Networks)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings = %+v, want none for a network that exists", plan.Warnings)
	}
}

func TestAMissingNetworkIsReportedWithTheExactCommandAndRefusedByInstall(t *testing.T) {
	in, stacks, _ := advancedInstaller(t)
	plan := previewWith(t, in, Advanced{NetworkMode: "iot"})
	if len(plan.Warnings) != 1 {
		t.Fatalf("warnings = %+v, want one", plan.Warnings)
	}
	w := plan.Warnings[0]
	if w.Class != WarnMissingNetwork || w.Detail != "iot" || w.Command != "docker network create iot" {
		t.Errorf("warning = %+v, want missing_network for iot with the exact command", w)
	}

	_, _, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin", Advanced: Advanced{NetworkMode: "iot"}})
	if !errors.Is(err, ErrNetworkMissing) || !strings.Contains(err.Error(), "docker network create iot") {
		t.Fatalf("Install error = %v, want ErrNetworkMissing naming the command", err)
	}
	if len(stacks.created) != 0 {
		t.Errorf("an install that refused created %d stacks", len(stacks.created))
	}
}

func TestAnErrorListingNetworksIsNeverReadAsTheNetworkExisting(t *testing.T) {
	in, stacks, engine := advancedInstaller(t)
	engine.SetNetworksError(container.ErrUnavailable)
	for name, run := range map[string]func() error{
		"preview": func() error {
			_, err := in.Preview(context.Background(), PlanRequest{ID: "jellyfin", Advanced: Advanced{NetworkMode: "lan"}})
			return err
		},
		"install": func() error {
			_, _, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin", Advanced: Advanced{NetworkMode: "lan"}})
			return err
		},
	} {
		if err := run(); !errors.Is(err, container.ErrUnavailable) {
			t.Errorf("%s error = %v, want the listing failure", name, err)
		}
	}
	if len(stacks.created) != 0 {
		t.Errorf("created %d stacks after a failed network check", len(stacks.created))
	}

	in.Networks = nil
	if _, err := in.Preview(context.Background(), PlanRequest{ID: "jellyfin", Advanced: Advanced{NetworkMode: "lan"}}); !errors.Is(err, container.ErrUnavailable) {
		t.Errorf("with no network source: error = %v, want ErrUnavailable", err)
	}
	if _, err := in.Preview(context.Background(), PlanRequest{ID: "jellyfin", Advanced: Advanced{NetworkMode: "host"}}); err != nil {
		t.Errorf("host needs no network list: %v", err)
	}
}

func TestAdvancedSettingsAreValidatedAndNameTheSettingThatFailed(t *testing.T) {
	in, stacks, _ := advancedInstaller(t)
	for _, tc := range []struct {
		name  string
		adv   Advanced
		input string
	}{
		{"network none", Advanced{NetworkMode: "none"}, "networkMode"},
		{"network with a colon", Advanced{NetworkMode: "container:abc"}, "networkMode"},
		{"network with a space", Advanced{NetworkMode: "my net"}, "networkMode"},
		{"network with shell syntax", Advanced{NetworkMode: "x$(id)"}, "networkMode"},
		{"network starting with a dash", Advanced{NetworkMode: "-lan"}, "networkMode"},
		{"one character network", Advanced{NetworkMode: "a"}, "networkMode"},
		{"network name too long", Advanced{NetworkMode: strings.Repeat("a", 65)}, "networkMode"},
		{"unknown restart policy", Advanced{Restart: "sometimes"}, "restart"},
		{"restart on-failure with a count", Advanced{Restart: "on-failure:5"}, "restart"},
		{"zero cpus given explicitly", Advanced{CPUs: f64(0)}, "cpus"},
		{"cpus below the minimum", Advanced{CPUs: f64(0.001)}, "cpus"},
		{"negative cpus", Advanced{CPUs: f64(-1)}, "cpus"},
		{"cpus above the maximum", Advanced{CPUs: f64(5000)}, "cpus"},
		{"zero memory given explicitly", Advanced{MemoryMiB: num(0)}, "memoryMiB"},
		{"memory below docker's minimum", Advanced{MemoryMiB: num(5)}, "memoryMiB"},
		{"negative memory", Advanced{MemoryMiB: num(-512)}, "memoryMiB"},
		{"memory above the maximum", Advanced{MemoryMiB: num(maxMemoryMiB + 1)}, "memoryMiB"},
		{"extra parameters too long", Advanced{ExtraParams: strings.Repeat("a", maxExtraParams+1)}, "extraParams"},
		{"extra parameters with a NUL", Advanced{ExtraParams: "--init\x00"}, "extraParams"},
		{"extra restart beside the setting", Advanced{ExtraParams: "--restart always", Restart: "no"}, "extraParams"},
		{"extra memory beside the setting", Advanced{ExtraParams: "--memory 1g", MemoryMiB: num(512)}, "extraParams"},
		{"extra cpus beside the setting", Advanced{ExtraParams: "--cpus 2", CPUs: f64(1)}, "extraParams"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for call, run := range map[string]func() error{
				"preview": func() error {
					_, err := in.Preview(context.Background(), PlanRequest{ID: "jellyfin", Advanced: tc.adv})
					return err
				},
				"install": func() error {
					_, _, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin", Advanced: tc.adv})
					return err
				},
			} {
				var ie *InputError
				err := run()
				if !errors.Is(err, ErrInvalidInput) || !errors.As(err, &ie) || ie.Input != tc.input {
					t.Errorf("%s error = %v, want an invalid input error for %s", call, err, tc.input)
				}
			}
			if len(stacks.created) != 0 {
				t.Errorf("created %d stacks for a refused request", len(stacks.created))
			}
		})
	}
}

func TestExtraParametersAreTranslatedAndWhatIsNotIsAWarningAndAComment(t *testing.T) {
	in, _, _ := advancedInstaller(t)
	plan := previewWith(t, in, Advanced{ExtraParams: "--dns 1.1.1.1 --some-exotic-flag=value --privileged"})
	svc := planService(t, plan.Compose, "jellyfin")
	if dns, _ := svc["dns"].([]any); len(dns) != 1 || dns[0] != "1.1.1.1" {
		t.Errorf("dns = %v, want the translated flag", svc["dns"])
	}
	untranslated := map[string]bool{}
	for _, w := range plan.Warnings {
		if w.Class == WarnUntranslatedFlag {
			untranslated[w.Detail] = true
		}
	}
	if !untranslated["--some-exotic-flag=value"] || !untranslated["--privileged"] || len(untranslated) != 2 {
		t.Errorf("untranslated warnings = %v, want both flags and no others", untranslated)
	}
	for _, line := range []string{"# Hoserva: could not translate the following extra parameters:", "#   --some-exotic-flag=value", "#   --privileged"} {
		if !strings.Contains(plan.Compose, line) {
			t.Errorf("Compose lacks the comment line %q:\n%s", line, plan.Compose)
		}
	}
	if svc["privileged"] != nil {
		t.Errorf("privileged = %v: an untranslated flag must not grant anything", svc["privileged"])
	}
	for _, p := range plan.Privileges {
		if p.Kind == PrivilegePrivileged {
			t.Errorf("privileges = %+v, want no privileged item for a flag that was not applied", plan.Privileges)
		}
	}
}

func TestExtraParametersThatWidenPrivilegesShowInThePrivilegeSummary(t *testing.T) {
	in, _, _ := advancedInstaller(t)
	plan := previewWith(t, in, Advanced{ExtraParams: "--cap-add NET_ADMIN --pid=host --device-cgroup-rule 'c 1:3 rmw' -v /:/host --group-add 44 --security-opt seccomp=unconfined -v /var/run/docker.sock:/var/run/docker.sock"})
	got := map[string]bool{}
	for _, p := range plan.Privileges {
		got[p.Kind] = true
	}
	for _, kind := range []string{PrivilegeAddedCaps, PrivilegeHostPID, PrivilegeDeviceCgroupRule, PrivilegeHostPath, PrivilegeGroupAdd, PrivilegeNoConfinement, PrivilegeDockerSocket} {
		if !got[kind] {
			t.Errorf("privileges = %v, want %s from the extra parameters", privilegeKinds(plan.Privileges), kind)
		}
	}
	if clean := previewWith(t, in, Advanced{}); len(clean.Privileges) != 0 {
		t.Errorf("without extra parameters privileges = %v, want none", privilegeKinds(clean.Privileges))
	}
}

func TestHostNetworkIsInThePrivilegeSummaryAndNotesThePortsItIgnores(t *testing.T) {
	in, _, _ := advancedInstaller(t)
	plan := previewWith(t, in, Advanced{NetworkMode: "host"})
	if got := strings.Join(privilegeKinds(plan.Privileges), "|"); got != "host_network:" {
		t.Errorf("privileges = %q, want host_network", got)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Class != WarnNote {
		t.Errorf("warnings = %+v, want a note about the unpublished ports", plan.Warnings)
	}
}

func TestExtraParameterTextIsNeverReadByAShell(t *testing.T) {
	in, _, _ := advancedInstaller(t)
	plan := previewWith(t, in, Advanced{ExtraParams: "--init; touch /tmp/pwned $(id) `id` > /etc/x"})
	if len(plan.Warnings) != 1 || plan.Warnings[0].Class != WarnUntranslatedFlag {
		t.Fatalf("warnings = %+v, want the whole string refused as one untranslated warning", plan.Warnings)
	}
	if svc := planService(t, plan.Compose, "jellyfin"); svc["init"] != nil {
		t.Errorf("init = %v: a refused string translates none of it", svc["init"])
	}
}

func TestExtraParameterValuesAreLiteralTextInCompose(t *testing.T) {
	in, _, _ := advancedInstaller(t)
	plan := previewWith(t, in, Advanced{ExtraParams: "-e 'TOKEN=a$b${C}'"})
	env, _ := planService(t, plan.Compose, "jellyfin")["environment"].(map[string]any)
	if env["TOKEN"] != "a$$b$${C}" {
		t.Errorf("TOKEN = %q, want its $ doubled so Compose reads it literally", env["TOKEN"])
	}
}

func TestExtraParametersThatClashWithTheTemplateKeepTheTemplatesAndWarn(t *testing.T) {
	in, _, _ := advancedInstaller(t)
	plan := previewWith(t, in, Advanced{ExtraParams: "-e PUID=1000 -e PGID=100 -e EXTRA=1 -v /srv/other:/config -v /srv/new:/new --user 1000"})
	svc := planService(t, plan.Compose, "jellyfin")
	env, _ := svc["environment"].(map[string]any)
	if env["PUID"] != "99" || env["EXTRA"] != "1" {
		t.Errorf("environment = %v, want the template's PUID kept and EXTRA added", env)
	}
	vols, _ := svc["volumes"].([]any)
	if len(vols) != 3 || vols[2] != "/srv/new:/new" {
		t.Errorf("volumes = %v, want the template's two and the new mount, not a second /config", vols)
	}
	classes := map[string]int{}
	for _, w := range plan.Warnings {
		classes[w.Class]++
	}
	if classes[WarnConflict] != 2 || classes[WarnNote] != 1 || classes[WarnFlaggedPath] == 0 {
		t.Errorf("warning classes = %v, want two conflicts (PUID, /config), a note (PGID repeats) and flagged paths", classes)
	}
}

func TestExtraParametersReplaceAScalarTheTemplateSetsAndSayWhich(t *testing.T) {
	in, _, _ := advancedInstaller(t)
	in.Catalog = MapCatalog{Templates: map[string]string{"probe": "services:\n  probe:\n    image: x\n    hostname: old\n    environment:\n      - A=1\n    labels: [\"a=1\"]\nx-hoserva:\n  schema: 1\n  id: probe\n  revision: 1\n  title: Probe\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n"}}
	plan, err := in.Preview(context.Background(), PlanRequest{ID: "probe", Advanced: Advanced{ExtraParams: "--hostname new -e B=2 -e A=2"}})
	if err != nil {
		t.Fatal(err)
	}
	svc := planService(t, plan.Compose, "probe")
	if svc["hostname"] != "new" {
		t.Errorf("hostname = %v, want the flag's value", svc["hostname"])
	}
	if env, _ := svc["environment"].([]any); len(env) != 2 || env[0] != "A=1" || env[1] != "B=2" {
		t.Errorf("environment = %v, want the list form kept with B added and A unchanged", svc["environment"])
	}
	var note, conflict bool
	for _, w := range plan.Warnings {
		note = note || (w.Class == WarnNote && strings.Contains(w.Message, "hostname"))
		conflict = conflict || (w.Class == WarnConflict && w.Detail == "A")
	}
	if !note || !conflict {
		t.Errorf("warnings = %+v, want a note for hostname and a conflict for A", plan.Warnings)
	}
}

func TestNetworkModeLimitsAndExtraParametersNeedASingleServiceButRestartDoesNot(t *testing.T) {
	in, stacks, _ := advancedInstaller(t)
	for _, adv := range []Advanced{{NetworkMode: "host"}, {CPUs: f64(1)}, {MemoryMiB: num(512)}, {ExtraParams: "--init"}} {
		_, err := in.Preview(context.Background(), PlanRequest{ID: "aio-notes", Advanced: adv})
		var ie *InputError
		if !errors.Is(err, ErrInvalidInput) || !errors.As(err, &ie) || ie.Input != adv.serviceSetting() {
			t.Errorf("Preview(%+v) error = %v, want %s refused", adv, err, adv.serviceSetting())
		}
	}
	plan, err := in.Preview(context.Background(), PlanRequest{ID: "aio-notes", Advanced: Advanced{Restart: "no"}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.AdvancedAvailable {
		t.Error("AdvancedAvailable is true for a template with three services")
	}
	for _, name := range []string{"app", "db", "cache"} {
		if got := planService(t, plan.Compose, name)["restart"]; got != "no" {
			t.Errorf("service %s restart = %v, want no", name, got)
		}
	}
	if single := previewWith(t, in, Advanced{}); !single.AdvancedAvailable {
		t.Error("AdvancedAvailable is false for a single-service template")
	}
	if len(stacks.created) != 0 {
		t.Errorf("created %d stacks", len(stacks.created))
	}
}

func TestInstallWritesTheComposeThePreviewShowedAndEchoesTheWarnings(t *testing.T) {
	in, stacks, _ := advancedInstaller(t)
	adv := Advanced{NetworkMode: "lan", Restart: "on-failure", CPUs: f64(0.5), MemoryMiB: num(256), ExtraParams: "--init --no-such-flag"}
	preview := previewWith(t, in, adv)
	plan, _, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin", Advanced: adv})
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks.created) != 1 || stacks.created[0].Compose != preview.Compose || plan.Compose != preview.Compose {
		t.Errorf("the installed Compose differs from the previewed one:\n%s\n---\n%s", stacks.created[0].Compose, preview.Compose)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Detail != "--no-such-flag" {
		t.Errorf("install warnings = %+v, want the untranslated flag", plan.Warnings)
	}
	svc := planService(t, plan.Compose, "jellyfin")
	if svc["restart"] != "on-failure" || svc["mem_limit"] != "256m" || svc["cpus"] != 0.5 || svc["init"] != true {
		t.Errorf("service = %v", svc)
	}
}

func TestPreviewListsEveryInputWhenARequiredOneIsEmptyAndInstallStillRefuses(t *testing.T) {
	in, stacks := newInstaller(t)
	in.Catalog = MapCatalog{Templates: map[string]string{"probe": "services:\n  probe:\n    image: x\n    ports: [\"${PORT}:80\"]\n    volumes: [\"${DATA}:/data\"]\n    environment:\n      NAME: ${NAME}\n      NOTE: ${NOTE}\nx-hoserva:\n  schema: 1\n  id: probe\n  revision: 1\n  title: Probe\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n  inputs:\n    DATA: { kind: path, role: share }\n    PORT: { kind: port }\n    NAME: { kind: string }\n    NOTE: { kind: string, optional: true }\n"}}
	plan, err := in.Preview(context.Background(), PlanRequest{ID: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Inputs) != 4 {
		t.Fatalf("inputs = %+v, want all four listed", plan.Inputs)
	}
	for name, want := range map[string]struct {
		required bool
		err      string
	}{"DATA": {true, "DATA needs a path"}, "PORT": {true, "PORT needs a port"}, "NAME": {true, "NAME needs a value"}, "NOTE": {false, ""}} {
		got := input(t, plan, name)
		if got.Required != want.required || got.Error != want.err {
			t.Errorf("%s = required %v, error %q, want %v, %q", name, got.Required, got.Error, want.required, want.err)
		}
	}
	_, _, err = in.Install(context.Background(), PlanRequest{ID: "probe"})
	var ie *InputError
	if !errors.Is(err, ErrInvalidInput) || !errors.As(err, &ie) || len(stacks.created) != 0 {
		t.Errorf("Install error = %v, created %d, want a refusal naming an input and nothing written", err, len(stacks.created))
	}

	filled, err := in.Preview(context.Background(), PlanRequest{ID: "probe", Values: map[string]string{"DATA": "/mnt/user/x", "PORT": "8080", "NAME": "n"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range filled.Inputs {
		if i.Error != "" {
			t.Errorf("input %s has the error %q although it has a value", i.Name, i.Error)
		}
	}
}

func TestAnInvalidValueStillRefusesThePreviewAndNamesTheInput(t *testing.T) {
	in, _ := newInstaller(t)
	_, err := in.Preview(context.Background(), PlanRequest{ID: "jellyfin", Values: map[string]string{"WEBUI_PORT": "70000"}})
	var ie *InputError
	if !errors.Is(err, ErrInvalidInput) || !errors.As(err, &ie) || ie.Input != "WEBUI_PORT" {
		t.Errorf("error = %v, want an invalid input error for WEBUI_PORT", err)
	}
	_, err = in.Preview(context.Background(), PlanRequest{ID: "jellyfin", Values: map[string]string{"NOPE": "x"}})
	if !errors.As(err, &ie) || ie.Input != "NOPE" {
		t.Errorf("error = %v, want an invalid input error for NOPE", err)
	}
}

func TestTheRestartPolicyNoIsQuotedSoNoReaderTakesItForFalse(t *testing.T) {
	in, _, _ := advancedInstaller(t)
	plan := previewWith(t, in, Advanced{Restart: "no"})
	if !strings.Contains(plan.Compose, `restart: "no"`) {
		t.Errorf("Compose does not quote the restart policy:\n%s", plan.Compose)
	}
}

// undefinedVolumes is the check `docker compose config` makes that a service
// refers only to volumes the file declares: every mount whose source is a
// name rather than a path.
func undefinedVolumes(t *testing.T, compose string) []string {
	t.Helper()
	var doc struct {
		Services map[string]struct {
			Volumes []any `yaml:"volumes"`
		} `yaml:"services"`
		Volumes map[string]any `yaml:"volumes"`
	}
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		t.Fatal(err)
	}
	var missing []string
	for _, svc := range doc.Services {
		for _, v := range svc.Volumes {
			var src string
			switch x := v.(type) {
			case string:
				if parts := strings.Split(x, ":"); len(parts) >= 2 {
					src = parts[0]
				}
			case map[string]any:
				if x["type"] == "volume" {
					src, _ = x["source"].(string)
				}
			}
			if src == "" || strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") || strings.HasPrefix(src, "$") {
				continue
			}
			if _, ok := doc.Volumes[src]; !ok {
				missing = append(missing, src)
			}
		}
	}
	return missing
}

func TestNamedVolumesInExtraParametersAreDeclaredAtTheTopLevel(t *testing.T) {
	in, _, _ := advancedInstaller(t)
	for name, flags := range map[string]string{
		"-v":      "-v jellyfin-cache:/cache",
		"--mount": "--mount type=volume,src=jellyfin-cache,dst=/cache",
	} {
		t.Run(name, func(t *testing.T) {
			plan := previewWith(t, in, Advanced{ExtraParams: flags})
			if !strings.Contains(plan.Compose, "jellyfin-cache") {
				t.Fatalf("the mount is not in the Compose file:\n%s", plan.Compose)
			}
			if missing := undefinedVolumes(t, plan.Compose); len(missing) != 0 {
				t.Errorf("volumes %v are used and not declared, which Compose refuses:\n%s", missing, plan.Compose)
			}
		})
	}
}

func TestANamedVolumeTheTemplateDeclaresKeepsItsDeclaration(t *testing.T) {
	in, _, _ := advancedInstaller(t)
	in.Catalog = MapCatalog{Templates: map[string]string{"vols": `services:
  vols:
    image: example/vols:1.0
    volumes:
      - data:/data
volumes:
  data:
    driver: local
    labels:
      note: kept
x-hoserva:
  schema: 1
  id: vols
  revision: 1
  title: Vols
  categories: [tools]
  icon: icon.svg
  docs: https://example.com/docs
`}}
	plan, err := in.Preview(context.Background(), PlanRequest{ID: "vols", Advanced: Advanced{ExtraParams: "-v data:/other -v extra:/more"}})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Volumes map[string]map[string]any `yaml:"volumes"`
	}
	if err := yaml.Unmarshal([]byte(plan.Compose), &doc); err != nil {
		t.Fatal(err)
	}
	if labels, _ := doc.Volumes["data"]["labels"].(map[string]any); labels["note"] != "kept" || doc.Volumes["data"]["driver"] != "local" {
		t.Errorf("the template's declaration of data = %v, want it kept", doc.Volumes["data"])
	}
	if _, ok := doc.Volumes["extra"]; !ok {
		t.Errorf("volumes = %v, want extra declared", doc.Volumes)
	}
	if missing := undefinedVolumes(t, plan.Compose); len(missing) != 0 {
		t.Errorf("volumes %v are used and not declared:\n%s", missing, plan.Compose)
	}
}

func TestExtraParameterPortsAreCheckedLikePortInputsAndNeverMoved(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		used  map[int]bool
		flags string
		bad   bool
	}{
		{"the template's own port again", nil, "-p 8096:8096", true},
		{"the template's own port on another container port", nil, "-p 8096:80", true},
		{"a port another container holds", map[int]bool{9000: true}, "-p 9000:80", true},
		{"a port held among several", map[int]bool{9000: true}, "-p 9100:80 -p 9000:81", true},
		{"a range that holds a taken port", map[int]bool{9001: true}, "-p 9000-9002:9000-9002", true},
		{"one host port twice", nil, "-p 9100:80 -p 9100:81", true},
		{"a free port", map[int]bool{9000: true}, "-p 9100:80", false},
		{"the template's port for another protocol", nil, "-p 8096:8096/udp", false},
		{"no host port", nil, "-p 80", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, stacks, _ := advancedInstaller(t)
			in.Ports = fakePorts{used: tc.used}
			adv := Advanced{ExtraParams: tc.flags}
			_, previewErr := in.Preview(ctx, PlanRequest{ID: "jellyfin", Advanced: adv})
			_, _, installErr := in.Install(ctx, PlanRequest{ID: "jellyfin", Advanced: adv})
			if !tc.bad {
				if previewErr != nil || installErr != nil || len(stacks.created) != 1 {
					t.Fatalf("preview %v, install %v, %d stacks, want a free port installed", previewErr, installErr, len(stacks.created))
				}
				return
			}
			for op, err := range map[string]error{"preview": previewErr, "install": installErr} {
				var ie *InputError
				if !errors.Is(err, ErrPortTaken) || !errors.As(err, &ie) || ie.Input != "extraParams" {
					t.Errorf("%s error = %v, want ErrPortTaken naming extraParams", op, err)
				}
			}
			if len(stacks.created) != 0 {
				t.Errorf("a refused install created %d stacks", len(stacks.created))
			}
		})
	}
}

func TestExtraParameterPortsAreRefusedWhenThePortsCannotBeListed(t *testing.T) {
	in, stacks, _ := advancedInstaller(t)
	// The template has no port input, so only the extra port needs the listing.
	in.Catalog = MapCatalog{Templates: map[string]string{"plain": "services:\n  plain:\n    image: x\nx-hoserva:\n  schema: 1\n  id: plain\n  revision: 1\n  title: Plain\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n"}}
	req := PlanRequest{ID: "plain", Advanced: Advanced{ExtraParams: "-p 9100:80"}}
	boom := errors.New("proc unreadable")
	in.Ports = fakePorts{err: boom}
	if _, _, err := in.Install(context.Background(), req); !errors.Is(err, boom) || len(stacks.created) != 0 {
		t.Fatalf("host ports unreadable: err = %v, %d stacks, want a refusal", err, len(stacks.created))
	}
	in.Ports = fakePorts{used: map[int]bool{}}
	stacks.portsErr = boom
	if _, _, err := in.Install(context.Background(), req); !errors.Is(err, boom) || len(stacks.created) != 0 {
		t.Fatalf("stack ports unreadable: err = %v, %d stacks, want a refusal", err, len(stacks.created))
	}
	stacks.portsErr = nil
	if _, _, err := in.Install(context.Background(), req); err != nil || len(stacks.created) != 1 {
		t.Fatalf("readable ports: err = %v, %d stacks, want it installed", err, len(stacks.created))
	}
}

func TestExtraParameterPortsUnderHostNetworkingAreNotChecked(t *testing.T) {
	in, _, _ := advancedInstaller(t)
	in.Ports = fakePorts{used: map[int]bool{9000: true}}
	if _, err := in.Preview(context.Background(), PlanRequest{ID: "jellyfin", Advanced: Advanced{NetworkMode: "host", ExtraParams: "-p 9000:80"}}); err != nil {
		t.Fatalf("a port Docker ignores under host networking was refused: %v", err)
	}
}
