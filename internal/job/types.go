// Package job implements the job system (doc 01 §4, #19): persisted,
// cancellable, observable long-running operations, with the mutually
// exclusive job classes enforced by Scheduler rather than trusted to the
// UI or CLI. Every long-running operation in Hoserva — sync, scrub,
// mover, disk format, container update, VM lifecycle — is a Job.
package job

import "fmt"

// Type is every job type named in doc 01 §4's mutually-exclusive-class
// table. It must match api/openapi.yaml's JobType enum exactly (#19) —
// classOf and resumableTypes below are this package's single source of
// truth for what class and resumability each one has; nothing else may
// declare that mapping.
type Type string

const (
	TypeSync              Type = "sync"
	TypeScrub             Type = "scrub"
	TypeFix               Type = "fix"
	TypeCheck             Type = "check"
	TypeRebalance         Type = "rebalance"
	TypeEvacuation        Type = "evacuation"
	TypeShareRelocation   Type = "share_relocation"
	TypeMover             Type = "mover"
	TypeVMDiskRelocation  Type = "vm_disk_relocation"
	TypeDiskFormat        Type = "disk_format"
	TypeDiskAdd           Type = "disk_add"
	TypeDiskRemove        Type = "disk_remove"
	TypeDiskReplace       Type = "disk_replace"
	TypeDiskUpgradeData   Type = "disk_upgrade_data"
	TypeDiskUpgradeParity Type = "disk_upgrade_parity"
	TypePoolRemount       Type = "pool_remount"
	TypeAppdataBackup     Type = "appdata_backup"
	TypeContainerUpdate   Type = "container_update"
	TypeACMEIssue         Type = "acme_issue"
	TypeVMStart           Type = "vm_start"
	TypeVMStop            Type = "vm_stop"
	TypeVMCreate          Type = "vm_create"
	TypeVMDelete          Type = "vm_delete"
	TypeVMSnapshot        Type = "vm_snapshot"
	TypeVMClone           Type = "vm_clone"
	TypeVMMigrationImport Type = "vm_migration_import"
)

// Class is the mutually exclusive job class the scheduler enforces
// (doc 01 §4).
type Class string

const (
	ClassParity     Class = "parity"
	ClassArrayWrite Class = "array_write"
	ClassTopology   Class = "topology"
	ClassService    Class = "service"
	ClassVM         Class = "vm"
)

// Status is a job's lifecycle state (doc 01 §4, api/openapi.yaml's
// JobStatus). interrupted is set only by a daemon restart or maintenance
// mode (Q70) — never cleared automatically; resuming or re-running is
// always an explicit user action (Q29).
type Status string

const (
	StatusQueued      Status = "queued"
	StatusRunning     Status = "running"
	StatusInterrupted Status = "interrupted"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusCancelled   Status = "cancelled"
)

// Terminal reports whether s is one of a job's terminal states — nothing
// leaves it without an explicit user action (Resume, doc 01 §4). Scheduler
// .Await (scheduler.go) uses this to know a step has actually finished, not
// just started.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	default:
		return false
	}
}

// classOf maps every job Type to its doc 01 §4 class. Deliberately a fixed
// table, not a caller-supplied field on JobSpec: the exclusion guarantees
// this package exists to enforce would mean nothing if a caller could name
// the wrong class for a job type.
var classOf = map[Type]Class{
	TypeSync:  ClassParity,
	TypeScrub: ClassParity,
	TypeFix:   ClassParity,
	TypeCheck: ClassParity,

	TypeRebalance:        ClassArrayWrite,
	TypeEvacuation:       ClassArrayWrite,
	TypeShareRelocation:  ClassArrayWrite,
	TypeMover:            ClassArrayWrite,
	TypeVMDiskRelocation: ClassArrayWrite,

	TypeDiskFormat:        ClassTopology,
	TypeDiskAdd:           ClassTopology,
	TypeDiskRemove:        ClassTopology,
	TypeDiskReplace:       ClassTopology,
	TypeDiskUpgradeData:   ClassTopology,
	TypeDiskUpgradeParity: ClassTopology,
	TypePoolRemount:       ClassTopology,

	TypeAppdataBackup:   ClassService,
	TypeContainerUpdate: ClassService,
	TypeACMEIssue:       ClassService,

	TypeVMStart:           ClassVM,
	TypeVMStop:            ClassVM,
	TypeVMCreate:          ClassVM,
	TypeVMDelete:          ClassVM,
	TypeVMSnapshot:        ClassVM,
	TypeVMClone:           ClassVM,
	TypeVMMigrationImport: ClassVM,
}

// resumableTypes are the only job types that persist a checkpoint to
// resume from (Q29): mover, rebalance, evacuation, share relocation, and
// the two disk upgrade types (#116, #289) — both wrap a resumable
// disk.RunDataDiskUpgrade/parity.RunParityUpgrade call that already
// checkpoints its own phase. Sync, scrub and fix are re-run, never
// resumed.
var resumableTypes = map[Type]bool{
	TypeMover:             true,
	TypeRebalance:         true,
	TypeEvacuation:        true,
	TypeShareRelocation:   true,
	TypeDiskUpgradeData:   true,
	TypeDiskUpgradeParity: true,
}

// ClassOf returns t's mutually exclusive class, and false if t is not a
// type this package knows about.
func ClassOf(t Type) (Class, bool) {
	c, ok := classOf[t]
	return c, ok
}

// Resumable reports whether t is one of Q29's resumable job types.
func Resumable(t Type) bool {
	return resumableTypes[t]
}

// ValidateType returns an error unless t is a known job Type.
func ValidateType(t Type) error {
	if _, ok := classOf[t]; !ok {
		return fmt.Errorf("job: unknown job type %q", t)
	}
	return nil
}
