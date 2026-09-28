// Package container is the Docker Engine subsystem abstraction (doc 01 §4,
// doc 04 §2-§3): nothing outside this package talks to the Engine API
// directly. Every caller — the API and, later, the CLI's dev mode — goes
// through Provider, so Apps is testable without a real Docker daemon (doc
// 06 §2). This first part covers a read-only client and prerequisite
// detection; start/stop/lifecycle actions and the Compose stack model are
// #277 and #278.
package container

import (
	"context"
	"errors"
	"time"
)

// ErrUnavailable is returned by every Provider method when the Docker
// Engine is not installed or not reachable (doc 04 §3) — never silently
// treated as "zero containers".
var ErrUnavailable = errors.New("container: docker engine is not reachable")

// ErrNotFound is returned by Inspect when id names no container the
// Engine knows about.
var ErrNotFound = errors.New("container: not found")

// ErrComposeUnavailable is returned by ComposeVersion when the Compose v2
// plugin is missing (doc 04 §3).
var ErrComposeUnavailable = errors.New("container: compose v2 plugin is not installed")

// EngineVersion is what Version reports about the reachable Docker Engine
// (Q38): the release string and the API version negotiated for every
// other call this Provider makes.
type EngineVersion struct {
	Version       string // e.g. "27.3.1"
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
	ID     string
	Name   string
	Image  string // repository, without its tag
	Tag    string
	State  string
	Status string
	Ports  []Port
	Mounts []Mount
}

// Image is one image the Docker Engine holds locally.
type Image struct {
	ID       string
	RepoTags []string
	Size     int64
	Created  time.Time
}

// Provider is the interface every subsystem touching the Docker Engine
// API sits behind (doc 01 §4, doc 06 §2): a real client, backed by the
// Engine API with a negotiated API version (Q38), and a scriptable fake
// for tests. Every method returns ErrUnavailable, wrapped, when the
// Engine cannot be reached — a caller must never read that as "no
// containers" (doc 04 §3: Apps shows a prerequisite banner instead).
type Provider interface {
	Version(ctx context.Context) (EngineVersion, error)
	List(ctx context.Context) ([]Container, error)
	Inspect(ctx context.Context, id string) (Container, error)
	Images(ctx context.Context) ([]Image, error)
}
