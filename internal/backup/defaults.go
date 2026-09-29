package backup

import "path/filepath"

// DefaultBootID and DefaultPoolID are the ids of Q40's two seeded
// destinations.
const (
	DefaultBootID = "boot"
	DefaultPoolID = "pool"
)

// isDefaultDestination reports whether d is one of the seeded defaults —
// local directories on this installation's own boot device and pool that
// no earlier release wrote to from anywhere else.
func isDefaultDestination(d Destination) bool {
	return !d.isRemote() && (d.ID == DefaultBootID || d.ID == DefaultPoolID)
}

// DefaultBootDestination is Q40's boot-device backup path.
const DefaultBootDestination = "/var/lib/hoserva/backups"

// DefaultPoolDestination is Q40's pool backup path — a dedicated directory
// on the mergerfs catch-all, covering the array failure domain.
const DefaultPoolDestination = "/mnt/user/hoserva-backups"

// DefaultRetentionDaily, DefaultRetentionWeekly and DefaultRetentionMonthly
// are doc 10 §1's per-destination retention defaults.
const (
	DefaultRetentionDaily   = 7
	DefaultRetentionWeekly  = 4
	DefaultRetentionMonthly = 6
)

// ArchiveVersion is the manifest's format version — bumped only when the
// on-disk archive layout changes.
const ArchiveVersion = 1

// DefaultDestinations returns Q40's two local destinations with doc 10 §1
// retention defaults.
func DefaultDestinations() []Destination {
	return []Destination{
		{
			ID:      DefaultBootID,
			Name:    "Boot device",
			Type:    TypeLocal,
			Path:    DefaultBootDestination,
			Enabled: true,
			Retention: Retention{
				Daily:   DefaultRetentionDaily,
				Weekly:  DefaultRetentionWeekly,
				Monthly: DefaultRetentionMonthly,
			},
		},
		{
			ID:      DefaultPoolID,
			Name:    "Pool",
			Type:    TypeLocal,
			Path:    DefaultPoolDestination,
			Enabled: true,
			Retention: Retention{
				Daily:   DefaultRetentionDaily,
				Weekly:  DefaultRetentionWeekly,
				Monthly: DefaultRetentionMonthly,
			},
		},
	}
}

// DefaultPaths derives archive source paths from production layout defaults.
func DefaultPaths(stateDir, configRoot string) Paths {
	if stateDir == "" {
		stateDir = "/var/lib/hoserva"
	}
	if configRoot == "" {
		configRoot = "/etc/hoserva"
	}
	return Paths{
		StateDir:            stateDir,
		ConfigRoot:          configRoot,
		DBPath:              filepath.Join(stateDir, "hoserva.db"),
		StacksDir:           filepath.Join(stateDir, "stacks"),
		TemplatesDir:        filepath.Join(stateDir, "templates"),
		SnapraidContentPath: filepath.Join(stateDir, "snapraid.content"),
	}
}
