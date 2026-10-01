package main

import (
	"errors"
	"slices"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func mockErrorCode(t *testing.T, err error) string {
	t.Helper()
	var me *mockError
	if !errors.As(err, &me) {
		t.Fatalf("expected *mockError, got %T: %v", err, err)
	}
	return me.code
}

// No scenario has a cache disk, so RemoveApp refuses appdata deletion the
// way hoservad does for an array without one — before it looks at whether
// the appdata is shared.
func TestRemoveAppRefusesAppdataWithoutACacheDisk(t *testing.T) {
	for _, id := range []string{"portainer", "transcoder"} {
		t.Run(id, func(t *testing.T) {
			h, err := newHandler("healthy")
			if err != nil {
				t.Fatalf("newHandler: %v", err)
			}
			_, err = h.RemoveApp(t.Context(), apiv1.RemoveAppParams{ID: id, DeleteAppdata: apiv1.NewOptBool(true)})
			if err == nil {
				t.Fatal("RemoveApp(deleteAppdata=true): expected appdata_unavailable, got success")
			}
			if code := mockErrorCode(t, err); code != "appdata_unavailable" {
				t.Fatalf("code = %q, want appdata_unavailable", code)
			}
			if _, err := h.findApp(id); err != nil {
				t.Fatalf("a refused removal must leave the container: %v", err)
			}
		})
	}
}

// Deleting appdata needs the array running, as in hoservad: the refusal is
// array_stopped, it comes before the container is looked up or the
// appdata location is considered, and it leaves the container in place. A
// plain remove is still allowed.
func TestRemoveAppRefusesAppdataWhileTheArrayIsStopped(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	h.maintenance = true

	for _, id := range []string{"portainer", "no-such-container"} {
		_, err = h.RemoveApp(t.Context(), apiv1.RemoveAppParams{ID: id, DeleteAppdata: apiv1.NewOptBool(true)})
		if err == nil {
			t.Fatalf("RemoveApp(%s, deleteAppdata=true) on a stopped array: expected array_stopped, got success", id)
		}
		if code := mockErrorCode(t, err); code != "array_stopped" {
			t.Fatalf("code = %q, want array_stopped", code)
		}
	}
	if _, err := h.findApp("portainer"); err != nil {
		t.Fatalf("a refused removal must leave the container: %v", err)
	}
	if _, err := h.RemoveApp(t.Context(), apiv1.RemoveAppParams{ID: "portainer"}); err != nil {
		t.Fatalf("RemoveApp without deleteAppdata on a stopped array: %v", err)
	}
}

func TestRemoveAppWithAppdataAvailable(t *testing.T) {
	t.Run("shared appdata is refused", func(t *testing.T) {
		h, err := newHandler("healthy")
		if err != nil {
			t.Fatalf("newHandler: %v", err)
		}
		_, err = h.removeApp(apiv1.RemoveAppParams{ID: "transcoder", DeleteAppdata: apiv1.NewOptBool(true)}, true)
		if err == nil {
			t.Fatal("expected appdata_shared, got success")
		}
		if code := mockErrorCode(t, err); code != "appdata_shared" {
			t.Fatalf("code = %q, want appdata_shared", code)
		}
	})

	t.Run("appdata is kept unless asked for, and a running container is refused", func(t *testing.T) {
		h, err := newHandler("healthy")
		if err != nil {
			t.Fatalf("newHandler: %v", err)
		}
		if _, err := h.removeApp(apiv1.RemoveAppParams{ID: "jellyfin"}, true); err == nil {
			t.Fatal("removing a running container should be refused")
		}
		res, err := h.removeApp(apiv1.RemoveAppParams{ID: "transcoder"}, true)
		if err != nil {
			t.Fatalf("removeApp without deleteAppdata: %v", err)
		}
		if len(res.DeletedPaths) != 0 {
			t.Fatalf("appdata must be kept unless asked for, deleted %v", res.DeletedPaths)
		}
	})

	t.Run("unshared appdata is reported deleted", func(t *testing.T) {
		h, err := newHandler("healthy")
		if err != nil {
			t.Fatalf("newHandler: %v", err)
		}
		mount := func(src string) apiv1.AppMount {
			return apiv1.AppMount{Source: apiv1.NewOptString(src), Destination: "/config"}
		}
		h.apps = []apiv1.App{
			{ID: "solo", Name: "solo", State: apiv1.AppStateExited, Mounts: []apiv1.AppMount{mount(mockAppdataRoot + "solo"), mount("/mnt/user/media")}},
			{ID: "other", Name: "other", State: apiv1.AppStateExited, Mounts: []apiv1.AppMount{mount(mockAppdataRoot + "other")}},
		}
		res, err := h.removeApp(apiv1.RemoveAppParams{ID: "solo", DeleteAppdata: apiv1.NewOptBool(true)}, true)
		if err != nil {
			t.Fatalf("removeApp(deleteAppdata=true): %v", err)
		}
		if want := []string{mockAppdataRoot + "solo"}; !slices.Equal(res.DeletedPaths, want) {
			t.Fatalf("DeletedPaths = %v, want %v", res.DeletedPaths, want)
		}
		if _, err := h.findApp("solo"); err == nil {
			t.Fatal("the container should be removed")
		}
		if _, err := h.findApp("other"); err != nil {
			t.Fatalf("another container must be left alone: %v", err)
		}
	})
}

func TestListAppsDistinguishesManagedFromUnmanagedContainers(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	list, err := h.ListApps(t.Context())
	if err != nil || !list.Available {
		t.Fatalf("ListApps = %+v, %v", list, err)
	}
	managed := map[string]string{}
	for _, a := range list.Apps {
		if s, ok := a.Stack.Get(); ok {
			managed[a.Name] = s
		}
	}
	if len(managed) != 1 || managed["jellyfin"] != mockStack {
		t.Fatalf("managed containers = %v, want only jellyfin in %s", managed, mockStack)
	}
	if _, err := h.GetStack(t.Context(), apiv1.GetStackParams{Name: mockStack}); err != nil {
		t.Fatalf("the stack listApps names is not one getStack knows: %v", err)
	}
	app, err := h.GetApp(t.Context(), apiv1.GetAppParams{ID: "jellyfin"})
	if err != nil || app.Stack.Value != mockStack {
		t.Fatalf("GetApp(jellyfin) = %+v, %v; want stack %s", app, err, mockStack)
	}
}

func TestFreshInstallScenarioHasDockerWithNoContainers(t *testing.T) {
	h, err := newHandler("fresh-install")
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	list, err := h.ListApps(t.Context())
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if !list.Available || len(list.Apps) != 0 {
		t.Fatalf("ListApps = %+v, want Docker available with no containers", list)
	}
	stacks, err := h.ListStacks(t.Context())
	if err != nil || len(stacks.Stacks) != 0 {
		t.Fatalf("ListStacks = %+v, %v; want none", stacks, err)
	}
}

func TestMigrationPendingScenarioHasNoDocker(t *testing.T) {
	h, err := newHandler("migration-pending")
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	list, err := h.ListApps(t.Context())
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if list.Available || !list.Message.Set || len(list.Apps) != 0 {
		t.Fatalf("ListApps = %+v, want available=false with a message", list)
	}
	if images, err := h.ListAppImages(t.Context()); err != nil || images.Available {
		t.Fatalf("ListAppImages = %+v, %v; want available=false", images, err)
	}
	if updates, err := h.ListAppUpdates(t.Context()); err != nil || updates.Available {
		t.Fatalf("ListAppUpdates = %+v, %v; want available=false", updates, err)
	}
	_, err = h.GetApp(t.Context(), apiv1.GetAppParams{ID: "jellyfin"})
	if code := mockErrorCode(t, err); code != "docker_unavailable" {
		t.Fatalf("GetApp code = %q, want docker_unavailable", code)
	}
}

// getApp is the one operation that reports when a container was created and
// started and how often it restarted, and the listing and the start, stop
// and restart responses, like hoservad's, report none of it nor the mounts'
// locations on the actions.
func TestGetAppCarriesTheContainersLifeAndOnlyGetAppDoes(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	app, err := h.GetApp(t.Context(), apiv1.GetAppParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if !app.CreatedAt.Set || !app.StartedAt.Set || app.RestartCount.Value != 1 {
		t.Fatalf("GetApp(jellyfin) = created %v started %v restarts %v, want all three set", app.CreatedAt, app.StartedAt, app.RestartCount)
	}

	list, err := h.ListApps(t.Context())
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	for _, a := range list.Apps {
		if a.CreatedAt.Set || a.StartedAt.Set || a.RestartCount.Set {
			t.Errorf("ListApps carries the life of %s", a.Name)
		}
	}

	started, err := h.StartApp(t.Context(), apiv1.StartAppParams{ID: "transcoder"})
	if err != nil {
		t.Fatalf("StartApp: %v", err)
	}
	if started.CreatedAt.Set || started.StartedAt.Set || started.RestartCount.Set {
		t.Errorf("StartApp carries the life of the container: %+v", started)
	}
	for _, m := range started.Mounts {
		if m.Location.Set {
			t.Errorf("StartApp places the mount %s", m.Destination)
		}
	}
	before, _ := h.GetApp(t.Context(), apiv1.GetAppParams{ID: "jellyfin"})
	after, err := h.GetApp(t.Context(), apiv1.GetAppParams{ID: "transcoder"})
	if err != nil || !after.StartedAt.Set || !after.StartedAt.Value.After(before.StartedAt.Value) {
		t.Fatalf("GetApp(transcoder) after a start = %+v, %v; want a start time later than jellyfin's", after, err)
	}
}

func TestMockAppsMountsCoverEveryStorageLocationKind(t *testing.T) {
	kinds := map[apiv1.AppMountLocationKind]bool{}
	for _, a := range mockApps() {
		for _, m := range a.Mounts {
			if loc, ok := m.Location.Get(); ok {
				kinds[loc.Kind] = true
			}
		}
	}
	for _, k := range []apiv1.AppMountLocationKind{apiv1.AppMountLocationKindPool, apiv1.AppMountLocationKindDisk, apiv1.AppMountLocationKindCache, apiv1.AppMountLocationKindOutside} {
		if !kinds[k] {
			t.Errorf("no mock container mounts a %s path", k)
		}
	}
}
