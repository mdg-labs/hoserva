// Package container is the Docker Engine subsystem abstraction (doc 01 §4,
// doc 04 §2-§3): nothing outside this package talks to the Engine API
// directly. Every caller — the API and, later, the CLI's dev mode — goes
// through Provider, so Apps is testable without a real Docker daemon (doc
// 06 §2). It covers listing and inspection, prerequisite detection and the
// lifecycle actions (start, stop, restart, recreate, remove, logs, stats,
// state events); the Compose stack model is #278.
package container

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrUnavailable is returned by every Provider method when the Docker
// Engine is not installed or not reachable (doc 04 §3) — never silently
// treated as "zero containers".
var ErrUnavailable = errors.New("container: docker engine is not reachable")

// ErrNotFound is returned by Inspect when id names no container the
// Engine knows about.
var ErrNotFound = errors.New("container: not found")

// ErrImageNotFound is returned by TagImage and UntagImage when the Engine
// has no such image or reference.
var ErrImageNotFound = errors.New("container: image not found")

// ErrImageInUse is returned by UntagImage when the reference is the image's
// last and a container still uses the image, so the Engine refuses to
// remove it.
var ErrImageInUse = errors.New("container: the image is in use by a container")

// ErrComposeUnavailable is returned by ComposeVersion when the Compose v2
// plugin is missing (doc 04 §3).
var ErrComposeUnavailable = errors.New("container: compose v2 plugin is not installed")

// EngineVersion is what Version reports about the reachable Docker Engine
// (Q38): the release string and the API version negotiated for every
// other call this Provider makes.
type EngineVersion struct {
	Version       string // e.g. "29.8.1"
	APIVersion    string // negotiated API version, e.g. "1.47"
	MinAPIVersion string
}

// Port is one container port mapping, as the Engine reports it.
type Port struct {
	HostIP        string
	HostPort      uint16 // zero when this container port is not published
	ContainerPort uint16
	Protocol      string // "tcp", "udp" or "sctp"
}

// Mount is one bind mount or volume attached to a container.
type Mount struct {
	Source      string
	Destination string
	Mode        string
	ReadWrite   bool
}

// Container is one Docker container Hoserva's Provider lists or inspects
// (doc 04 §2, §3): read-only fields only. State uses the Engine's own
// vocabulary (created, running, paused, restarting, removing, exited,
// dead) — Hoserva never invents a second one for it (CLAUDE.md: one
// placement/state model per external tool). Nothing here distinguishes
// "managed" from "unmanaged": that needs the stacks table #278 adds: every
// container this part lists is, today, honestly unmanaged, since nothing
// in Hoserva has installed one yet.
type Container struct {
	ID    string
	Name  string
	Image string // repository, without its tag
	Tag   string
	// ImageID is the local image the container runs, matched against
	// Image.ID to find the registry digest it was pulled as.
	ImageID string
	// Pinned is set for a container created from a digest reference
	// ("nginx@sha256:..."): it runs exactly that image, so there is no tag
	// to look for an update of.
	Pinned bool
	State  string
	Status string
	Health string // one of the Health constants
	Ports  []Port
	Mounts []Mount
	// Labels are the container's Engine labels; StackService reads the
	// Compose project label to find a stack's containers.
	Labels map[string]string
}

// composeProjectLabel is the label Docker Compose puts on every container
// of a project, the project being the stack's name.
const composeProjectLabel = "com.docker.compose.project"

// composeWorkingDirLabel and composeConfigFilesLabel record where Compose
// started a container from: the project directory and the comma-separated
// compose files. Two projects can share a name, and these tell them apart.
const (
	composeWorkingDirLabel  = "com.docker.compose.project.working_dir"
	composeConfigFilesLabel = "com.docker.compose.project.config_files"
)

// Runtime is the timing and restart facts of one container, from the
// Engine's inspection of it.
type Runtime struct {
	CreatedAt time.Time
	// StartedAt is the zero time for a container that has never run.
	StartedAt    time.Time
	RestartCount int
}

// Image is one image the Docker Engine holds locally.
type Image struct {
	ID       string
	RepoTags []string
	// RepoDigests are the registry manifest digests the image was pulled
	// as, each "repository@sha256:...".
	RepoDigests []string
	Size        int64
	Created     time.Time
}

// ConfiguredImage is a container's image reference as its configuration names
// it.
type ConfiguredImage struct {
	// Ref is "repository:tag" in the familiar form the update check keys
	// images by ("nginx:latest", "ghcr.io/owner/app:1.2"); for a Pinned
	// container it is the reference as configured.
	Ref string
	// Pinned is set for a reference that names a digest.
	Pinned bool
}

// configuredImage parses the reference a container was created with.
func configuredImage(raw string) ConfiguredImage {
	repo, tag := splitImageRef(raw)
	if isDigestPinned(raw) {
		return ConfiguredImage{Ref: raw, Pinned: true}
	}
	ref, err := ParseImageRef(repo, tag)
	if err != nil {
		return ConfiguredImage{Ref: raw}
	}
	return ConfiguredImage{Ref: ref.String()}
}

// ErrRunning is returned by Remove for a container that is not stopped:
// removing a live container would kill it mid-write.
var ErrRunning = errors.New("container: the container is running")

// ErrNotRunning is returned by Stats for a container that is not running,
// so a stopped container is never reported as using zero CPU and memory.
var ErrNotRunning = errors.New("container: the container is not running")

// Container health, in the Engine's own vocabulary (HEALTHCHECK). "none"
// means the container defines no health check, or the Engine reported
// none for it.
const (
	HealthNone      = "none"
	HealthStarting  = "starting"
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)

// Stats is one point-in-time resource reading for a running container.
type Stats struct {
	At               time.Time
	CPUPercent       float64
	MemoryBytes      uint64
	MemoryLimitBytes uint64
	NetworkRxBytes   uint64
	NetworkTxBytes   uint64
	BlockReadBytes   uint64
	BlockWriteBytes  uint64
}

// LogOptions selects which part of a container's output Logs returns.
type LogOptions struct {
	// Tail is the number of trailing lines to start from; negative means
	// every line the Engine still holds.
	Tail int
	// Follow keeps the stream open and delivers new lines as they are
	// written, until the context is cancelled or the container exits.
	Follow bool
}

// RemoveOptions selects what Remove deletes beyond the container itself.
type RemoveOptions struct {
	// Volumes also removes the container's anonymous volumes. Named
	// volumes and bind mounts are never touched by the Engine.
	Volumes bool
}

// StateChange is one container state or health transition, as the Engine
// reported it or as an API action just observed it. Health is empty when
// the change carries no health information.
type StateChange struct {
	ID     string
	Name   string
	State  string
	Health string
	At     time.Time
}

// Provider is the interface every subsystem touching the Docker Engine
// API sits behind (doc 01 §4, doc 06 §2): a real client, backed by the
// Engine API with a negotiated API version (Q38), and a scriptable fake
// for tests. Every method returns ErrUnavailable, wrapped, when the
// Engine cannot be reached — a caller must never read that as "no
// containers" (doc 04 §3: Apps shows a prerequisite banner instead).
// Lifecycle methods take a container's Engine ID or name, matched exactly
// like Inspect, and return ErrNotFound for anything else.
type Provider interface {
	Version(ctx context.Context) (EngineVersion, error)
	List(ctx context.Context) ([]Container, error)
	Inspect(ctx context.Context, id string) (Container, error)
	// ConfiguredImage is the image reference the container was created
	// with. Inspect and List report the local image's ID in its place once
	// the reference points at a different image than the container runs
	// (the Engine's own container listing does), so an update, which moves
	// that reference, reads it here.
	ConfiguredImage(ctx context.Context, id string) (ConfiguredImage, error)
	// ConfiguredPorts are the host ports the container is configured to
	// publish. List and Inspect report published ports only for a container
	// that is running, but a stopped one binds the same ports again when it
	// starts, so the configuration is read from the container's own
	// inspection. A port left to the Engine to choose has a zero HostPort.
	ConfiguredPorts(ctx context.Context, id string) ([]Port, error)
	Images(ctx context.Context) ([]Image, error)
	// StartedAt is when the Engine last started the container, in UTC, and
	// the zero time for one it has never started. The listing does not carry
	// it, so it is an inspection of its own, and an error is never a time:
	// a caller that cannot learn it must not assume the container did not
	// run.
	StartedAt(ctx context.Context, id string) (time.Time, error)
	// CreatedAt is when the Engine created the container, in UTC. A
	// recreation makes a new container, so this is later than any start of
	// the one it replaced. An error is never a time, and a container the
	// Engine gives no creation time for is an error too.
	CreatedAt(ctx context.Context, id string) (time.Time, error)
	// Runtime is what one inspection of the container reports about its
	// life: when it was created and last started and how often the Engine
	// restarted it. The errors of CreatedAt and StartedAt apply: a missing
	// creation time is an error, and an error is never a time or a count.
	Runtime(ctx context.Context, id string) (Runtime, error)

	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Restart(ctx context.Context, id string) error
	// Remove returns ErrRunning for a container that is not stopped.
	Remove(ctx context.Context, id string, opts RemoveOptions) error
	// Recreate pulls the container's image again and replaces the
	// container with one built from the same configuration, volumes and
	// networks. A failure at any step leaves the original container as it
	// was: same name, configuration and volumes, running if it was running.
	Recreate(ctx context.Context, id string) error
	// PullImage downloads the image ref names ("repository:tag") and points
	// the tag at what the registry serves now, which leaves the image it
	// pointed at before in place and untagged. Nothing else changes: no
	// container is touched.
	PullImage(ctx context.Context, ref string) error
	// RecreateLocal is Recreate without the pull: the replacement uses the
	// image the container's reference names locally, with the same
	// guarantee that a failure leaves the original as it was.
	RecreateLocal(ctx context.Context, id string) error
	// TagImage gives the local image with this ID the reference ref, moving
	// the tag off any other image that holds it. It returns
	// ErrImageNotFound for an unknown image.
	TagImage(ctx context.Context, imageID, ref string) error
	// UntagImage removes the reference ref and returns ErrImageNotFound when
	// no image has it. The Engine deletes the image only when ref was its
	// last reference and no container uses it; for one that is used it
	// returns ErrImageInUse and keeps the reference.
	UntagImage(ctx context.Context, ref string) error
	// Reconcile finishes or undoes a Recreate that was cut short by the
	// daemon dying, and reports what it found (see EngineClient.Reconcile).
	// It may start a container, so it runs only while the array is up.
	Reconcile(ctx context.Context) ([]Reconciliation, error)
	// Logs returns the container's stdout and stderr as plain text. The
	// caller closes the reader; cancelling ctx also ends a follow.
	Logs(ctx context.Context, id string, opts LogOptions) (io.ReadCloser, error)
	// Stats returns ErrNotRunning for a container that is not running.
	Stats(ctx context.Context, id string) (Stats, error)
	// Watch calls fn for every container state or health change the Engine
	// reports, until ctx is cancelled or the event stream fails. It never
	// polls: a killed container arrives as an event.
	Watch(ctx context.Context, fn func(StateChange)) error
}
