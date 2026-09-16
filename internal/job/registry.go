package job

import (
	"fmt"
	"sync"
)

// ErrJobTypeNotRegistered is returned by Submit and Resume for a job Type
// no issue has wired to a real RunFunc yet (doc 01 §4). Every type this
// package knows about is valid (ValidateType passes); not every type has
// something to actually run.
var ErrJobTypeNotRegistered = fmt.Errorf("job: this job type has no registered implementation yet")

type registryEntry struct {
	run         RunFunc
	cancellable bool
}

// Registry binds each job Type to the RunFunc that actually performs it,
// and to whether its underlying tool honestly supports being cancelled
// (doc 01 §4) — SnapRAID's sync, the mover walking the pool, a container
// pull, and so on, each wired once by the issue that implements it against
// a real subsystem. Cancellable is deliberately not a per-Submit-call
// choice: doc 01 §4 makes it a property of the tool a job type runs, never
// of who happens to be asking, so Registry is the only place it is ever
// set.
type Registry struct {
	mu      sync.RWMutex
	entries map[Type]registryEntry
}

// NewRegistry returns an empty Registry — nothing runs until Register is
// called for a type.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[Type]registryEntry)}
}

// Register binds t to run and cancellable. Registering the same type
// twice is a startup wiring bug (two issues both claiming the same job
// type), so it panics rather than silently letting the second call
// replace the first.
func (r *Registry) Register(t Type, cancellable bool, run RunFunc) {
	if err := ValidateType(t); err != nil {
		panic(err)
	}
	if run == nil {
		panic(fmt.Sprintf("job: Registry.Register(%s): run is nil", t))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[t]; exists {
		panic(fmt.Sprintf("job: type %q is already registered", t))
	}
	r.entries[t] = registryEntry{run: run, cancellable: cancellable}
}

func (r *Registry) lookup(t Type) (registryEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[t]
	return e, ok
}
