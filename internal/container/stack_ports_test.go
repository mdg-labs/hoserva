package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/network"

	"github.com/mdg-labs/hoserva/internal/store"
)

const composeConfigWebJSON = `{"name":"web","services":{"web":{"image":"nginx","ports":[` +
	`{"mode":"ingress","target":80,"published":"8096","protocol":"tcp"},` +
	`{"mode":"ingress","host_ip":"127.0.0.1","target":70,"published":"7000","protocol":"udp"},` +
	`{"mode":"ingress","target":81,"protocol":"tcp"}]},` +
	`"worker":{"image":"nginx","ports":[{"mode":"ingress","target":9000,"published":"9000-9001","protocol":"tcp"}]},` +
	`"idle":{"image":"nginx"}}}`

func portKeys(m map[int]bool) []int {
	var out []int
	for p := range m {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

func (r *stackRig) scriptConfig(name, output string, err error) {
	r.runner.Script("docker", r.composeArgv(name, "--profile", "*", "config", "--format", "json"), []byte(output), err)
}

func TestStackPublishedPorts_ReadsEveryStacksResolvedPortsWithoutStartingAnything(t *testing.T) {
	clearComposeEnv(t)
	r := newStackRig(t)
	r.create(t, "web")
	r.create(t, "other")
	r.scriptConfig("web", composeConfigWebJSON, nil)
	r.scriptConfig("other", `{"services":{"db":{"ports":[{"target":5432,"published":"5432","protocol":"tcp"}]}}}`, nil)
	before := len(r.runner.Calls())

	got, err := r.svc.PublishedPorts(context.Background())
	if err != nil {
		t.Fatalf("PublishedPorts: %v", err)
	}
	if want := []int{5432, 7000, 8096, 9000, 9001}; !reflect.DeepEqual(portKeys(got), want) {
		t.Errorf("ports = %v, want %v: a port the Engine chooses is not taken, a range is every port in it", portKeys(got), want)
	}
	for _, c := range r.runner.Calls()[before:] {
		if !strings.Contains(strings.Join(c.Args, " "), "config --format json") {
			t.Errorf("PublishedPorts ran %v, want only `compose config`", c.Args)
		}
		if c.Env == nil {
			t.Errorf("%v ran with the daemon's whole environment", c.Args)
		}
		envFile := filepath.Join(r.root, c.Args[2], ".env")
		if !strings.Contains(strings.Join(c.Args, " "), "--env-file "+envFile) {
			t.Errorf("%v does not resolve the stack with its own .env", c.Args)
		}
	}
}

func TestStackPublishedPorts_UsesTheEnvFileOnDisk(t *testing.T) {
	clearComposeEnv(t)
	r := newStackRig(t)
	r.create(t, "web")
	r.scriptConfig("web", composeConfigWebJSON, nil)
	if err := os.WriteFile(filepath.Join(r.root, "web", ".env"), []byte("WEBUI_PORT=8100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PublishedPorts(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := r.runner.Calls()
	last := calls[len(calls)-1]
	if want := filepath.Join(r.root, "web", ".env"); !strings.Contains(strings.Join(last.Args, " "), "--env-file "+want) {
		t.Errorf("argv = %v, want the .env on disk", last.Args)
	}
}

func TestStackPublishedPorts_NoStacksIsAnEmptySetNotAnError(t *testing.T) {
	r := newStackRig(t)
	got, err := r.svc.PublishedPorts(context.Background())
	if err != nil || len(got) != 0 || len(r.runner.Calls()) != 0 {
		t.Fatalf("got %v, %v after %d calls", got, err, len(r.runner.Calls()))
	}
}

func TestStackPublishedPorts_FailsWhenAnyStacksPortsCannotBeRead(t *testing.T) {
	clearComposeEnv(t)
	for name, tc := range map[string]struct {
		output string
		err    error
		want   error
	}{
		"compose fails":        {err: errors.New("variable is not set"), want: ErrComposeFailed},
		"output is not JSON":   {output: "services:", want: nil},
		"no output":            {output: "", want: nil},
		"unexpected port":      {output: `{"services":{"web":{"ports":[{"published":"web"}]}}}`, want: nil},
		"port out of range":    {output: `{"services":{"web":{"ports":[{"published":"70000"}]}}}`, want: nil},
		"range runs backwards": {output: `{"services":{"web":{"ports":[{"published":"9001-9000"}]}}}`, want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			r := newStackRig(t)
			r.create(t, "good")
			r.create(t, "web")
			r.scriptConfig("good", `{"services":{}}`, nil)
			r.scriptConfig("web", tc.output, tc.err)
			got, err := r.svc.PublishedPorts(context.Background())
			if err == nil {
				t.Fatalf("PublishedPorts = %v, nil: a stack whose ports could not be read must not leave its ports free", got)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), "stack web") {
				t.Errorf("err = %v, want the stack named", err)
			}
		})
	}
}

func TestStackPublishedPorts_WritesAMissingFileFromItsRowAndNeverOverwritesOne(t *testing.T) {
	clearComposeEnv(t)
	r := newStackRig(t)
	r.create(t, "web")
	r.scriptConfig("web", composeConfigWebJSON, nil)
	dir := filepath.Join(r.root, "web")
	if err := os.Remove(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("HAND=written\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PublishedPorts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(t, filepath.Join(dir, "meta.json")) {
		t.Error("the missing meta.json was not written from the row")
	}
	if got := string(readFile(t, filepath.Join(dir, ".env"))); got != "HAND=written\n" {
		t.Errorf(".env = %q, a file that exists is never overwritten", got)
	}
}

func TestStackPublishedPorts_AnEnvTheRowCannotOpenRefusesInsteadOfSkippingTheStack(t *testing.T) {
	clearComposeEnv(t)
	r := newStackRig(t)
	r.create(t, "web")
	r.svc.Cipher = unopenableCipher{}
	if err := os.Remove(filepath.Join(r.root, "web", ".env")); err != nil {
		t.Fatal(err)
	}
	if got, err := r.svc.PublishedPorts(context.Background()); err == nil {
		t.Fatalf("PublishedPorts = %v, nil, want an error for a stack whose .env is gone and cannot be regenerated", got)
	}
}

// staleListStore lists rows the table no longer has, as a Remove or a failed
// create that finished after the listing was taken leaves it.
type staleListStore struct {
	StackStore
	listed []store.Stack
}

func (s staleListStore) List(context.Context) ([]store.Stack, error) { return s.listed, nil }

func TestStackPublishedPorts_AStackRemovedAfterTheListingHoldsNoPortAndRefusesNothing(t *testing.T) {
	clearComposeEnv(t)
	r := newStackRig(t)
	r.create(t, "web")
	r.create(t, "gone")
	r.scriptConfig("web", composeConfigWebJSON, nil)
	listed, err := r.store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.svc.Store = staleListStore{StackStore: r.store, listed: listed}
	if err := r.store.Delete(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(r.root, "gone")); err != nil {
		t.Fatal(err)
	}

	got, err := r.svc.PublishedPorts(context.Background())
	if err != nil {
		t.Fatalf("PublishedPorts: %v, want the removed stack skipped", err)
	}
	if want := []int{7000, 8096, 9000, 9001}; !reflect.DeepEqual(portKeys(got), want) {
		t.Errorf("ports = %v, want %v: the stacks that still exist are read", portKeys(got), want)
	}
	if exists(t, filepath.Join(r.root, "gone")) {
		t.Error("files were written back for a stack whose row is gone")
	}
}

func TestStackPublishedPorts_DoesNotWaitForAnUpInProgress(t *testing.T) {
	clearComposeEnv(t)
	r := newStackRig(t)
	r.create(t, "web")
	r.scriptConfig("web", composeConfigWebJSON, nil)
	r.svc.mu.Lock()
	defer r.svc.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := r.svc.PublishedPorts(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PublishedPorts while the stack lock is held: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PublishedPorts waited for the stack lock, which an Up holds while it pulls")
	}
}

func TestEngineConfiguredPorts_ReadsTheBindingsOfAStoppedContainer(t *testing.T) {
	e := newScriptedEngine(false)
	e.info.HostConfig.PortBindings = network.PortMap{
		network.MustParsePort("8096/tcp"): {{HostPort: "8096"}},
		network.MustParsePort("53/udp"):   {{HostPort: "5353"}, {HostPort: "0"}},
		network.MustParsePort("80/tcp"):   {{HostPort: ""}},
		network.MustParsePort("9000/tcp"): {{HostPort: "9100-9101"}},
	}
	c := &EngineClient{cli: e}
	got, err := c.ConfiguredPorts(context.Background(), "jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	var host []int
	for _, p := range got {
		if p.HostPort != 0 {
			host = append(host, int(p.HostPort))
		}
	}
	if want := []int{5353, 8096, 9100, 9101}; !reflect.DeepEqual(host, want) {
		t.Errorf("host ports = %v, want %v (ports the Engine chooses are zero, a range is every port)", host, want)
	}
	for _, p := range got {
		if p.HostPort == 5353 && (p.ContainerPort != 53 || p.Protocol != "udp") {
			t.Errorf("port = %+v", p)
		}
	}
}

func TestEngineConfiguredPorts_RefusesWhatItCannotRead(t *testing.T) {
	e := newScriptedEngine(false)
	c := &EngineClient{cli: e}
	e.info.HostConfig.PortBindings = network.PortMap{network.MustParsePort("80/tcp"): {{HostPort: "web"}}}
	if got, err := c.ConfiguredPorts(context.Background(), "jellyfin"); err == nil {
		t.Errorf("an unreadable host port gave %v, nil", got)
	}
	e.info.HostConfig = nil
	if got, err := c.ConfiguredPorts(context.Background(), "jellyfin"); err == nil {
		t.Errorf("no host configuration gave %v, nil", got)
	}
	e.info.HostConfig = nil
	if _, err := c.ConfiguredPorts(context.Background(), "other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a prefix or other name: err = %v, want ErrNotFound", err)
	}
}
