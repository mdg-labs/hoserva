package pool

import (
	"strings"
	"testing"
)

func TestMount_Render(t *testing.T) {
	m := Mount{
		Where:             "/mnt/user/movies",
		What:              "/mnt/cache/movies=RW:/mnt/disk1/movies=NC",
		FSName:            "hoserva-movies",
		CreatePolicy:      KeepFoldersTogether,
		Options:           DefaultOptions(),
		Description:       "Hoserva share movies",
		RequiresMountsFor: []string{"/mnt/user", "/mnt/cache", "/mnt/disk1"},
	}
	got := m.Render()

	for _, want := range []string{
		"Description=Hoserva share movies",
		"RequiresMountsFor=/mnt/user /mnt/cache /mnt/disk1",
		"What=/mnt/cache/movies=RW:/mnt/disk1/movies=NC",
		"Where=/mnt/user/movies",
		"Type=fuse.mergerfs",
		"Options=category.create=mspmfs,moveonenospc=true,dropcacheonclose=true,minfreespace=50G,cache.files=partial,cache.entry=1,cache.attr=1,cache.negative_entry=1,cache.statfs=0,fsname=hoserva-movies",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Render() = %q, want it to contain %q", got, want)
		}
	}
}

func TestMount_Render_OmitsRequiresMountsForWhenEmpty(t *testing.T) {
	m := Mount{Where: "/mnt/user", What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	got := m.Render()
	if strings.Contains(got, "RequiresMountsFor") {
		t.Fatalf("Render() = %q, want no RequiresMountsFor line when none is set", got)
	}
}

func TestMount_Argv(t *testing.T) {
	m := Mount{
		Where:        "/mnt/user",
		What:         "/mnt/disk1=RW:/mnt/disk2=RW",
		FSName:       "hoserva-pool",
		CreatePolicy: DefaultCreatePolicy,
		Options:      DefaultOptions(),
	}
	argv := m.Argv()
	if argv[0] != "mergerfs" {
		t.Fatalf("Argv()[0] = %q, want mergerfs", argv[0])
	}
	if argv[1] != "-o" {
		t.Fatalf("Argv()[1] = %q, want -o", argv[1])
	}
	if argv[len(argv)-2] != m.What || argv[len(argv)-1] != m.Where {
		t.Fatalf("Argv() = %v, want it to end with branches then mountpoint", argv)
	}
	for _, a := range argv {
		if a == "-f" {
			t.Fatalf("Argv() = %v, want no -f — mergerfs should daemonize on its own", argv)
		}
	}
}
