package container

import (
	"reflect"
	"testing"
)

func TestUsingPaths(t *testing.T) {
	roots := []string{"/mnt/user/appdata", "/mnt/cache/appdata", "/mnt/disk1/appdata", "/mnt/disk2/appdata"}
	mount := func(src string) Mount { return Mount{Source: src, Destination: "/x"} }
	containers := []Container{
		{ID: "1", Name: "db", State: "running", Mounts: []Mount{mount("/mnt/user/appdata/postgres"), mount("/etc/localtime")}},
		{ID: "2", Name: "media", State: "running", Mounts: []Mount{mount("/mnt/user/media")}},
		{ID: "3", Name: "backup", State: "exited", Mounts: []Mount{mount("/mnt/user")}},
		{ID: "4", Name: "direct", State: "paused", Mounts: []Mount{mount("/mnt/cache/appdata/"), mount("/mnt/disk2/appdata/nested/dir")}},
		{ID: "5", Name: "lookalike", State: "running", Mounts: []Mount{mount("/mnt/user/appdata2"), mount("/mnt/disk10/appdata")}},
		{ID: "6", Name: "volume", State: "running", Mounts: []Mount{mount(""), mount("data"), mount("/var/lib/docker/volumes/x/_data")}},
		{ID: "7", Name: "restarting", State: "restarting", Mounts: []Mount{mount("/mnt/user/appdata")}},
		{ID: "8", Name: "dotted", State: "created", Mounts: []Mount{mount("/mnt/user/media/../appdata/x")}},
	}

	got := UsingPaths(containers, roots)

	type row struct {
		Name    string
		Active  bool
		Sources []string
	}
	var rows []row
	for _, u := range got {
		rows = append(rows, row{u.Container.Name, u.Active, u.Sources})
	}
	want := []row{
		{"db", true, []string{"/mnt/user/appdata/postgres"}},
		{"backup", false, []string{"/mnt/user"}},
		{"direct", true, []string{"/mnt/cache/appdata/", "/mnt/disk2/appdata/nested/dir"}},
		{"restarting", true, []string{"/mnt/user/appdata"}},
		{"dotted", false, []string{"/mnt/user/media/../appdata/x"}},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("UsingPaths = %+v, want %+v", rows, want)
	}
}

func TestUsingPaths_NoRootsMatchesNothing(t *testing.T) {
	c := []Container{{ID: "1", Name: "all", State: "running", Mounts: []Mount{{Source: "/"}}}}
	if got := UsingPaths(c, nil); len(got) != 0 {
		t.Fatalf("UsingPaths with no roots = %+v, want none", got)
	}
	if got := UsingPaths(c, []string{""}); len(got) != 0 {
		t.Fatalf("UsingPaths with an empty root = %+v, want none", got)
	}
}
