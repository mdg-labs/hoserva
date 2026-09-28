package container

import (
	"reflect"
	"testing"

	"github.com/mdg-labs/hoserva/internal/store"
)

func TestCacheAppdataRoots(t *testing.T) {
	data := store.ArrayDisk{Role: store.ArrayRoleData, RoleIndex: 1, Mountpoint: "/mnt/disk1"}
	parity := store.ArrayDisk{Role: store.ArrayRoleParity, RoleIndex: 1, Mountpoint: "/mnt/parity"}
	cache := store.ArrayDisk{Role: store.ArrayRoleCache, RoleIndex: 1, Mountpoint: "/mnt/cache"}

	tests := []struct {
		name  string
		disks []store.ArrayDisk
		want  []string
	}{
		{"no disks", nil, nil},
		{"data and parity only", []store.ArrayDisk{data, parity}, nil},
		{"cache disk without a mountpoint", []store.ArrayDisk{data, {Role: store.ArrayRoleCache, RoleIndex: 1}}, nil},
		{"cache disk", []store.ArrayDisk{data, parity, cache}, []string{"/mnt/cache/appdata"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CacheAppdataRoots(tc.disks); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("CacheAppdataRoots = %v, want %v", got, tc.want)
			}
		})
	}
}
