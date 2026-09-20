package backup

import "github.com/mdg-labs/hoserva/internal/disk"

// ExternalDestination is a local config-backup target on an external
// disk's mount (doc 10 §1, Q72). Path is /mnt/disks/<label> — the same
// stable path a container bind-mount uses.
func ExternalDestination(label string) (Destination, error) {
	path, err := disk.ExternalMountPoint(label)
	if err != nil {
		return Destination{}, err
	}
	return Destination{
		ID:      "external:" + label,
		Path:    path,
		Enabled: true,
		Retention: Retention{
			Daily:   DefaultRetentionDaily,
			Weekly:  DefaultRetentionWeekly,
			Monthly: DefaultRetentionMonthly,
		},
	}, nil
}
