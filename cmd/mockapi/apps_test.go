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
