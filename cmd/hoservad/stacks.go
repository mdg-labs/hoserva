package main

import (
	"path/filepath"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
)

// stacksDirName is the stacks directory inside the state directory
// (backup.DefaultPaths' StacksDir): /var/lib/hoserva/stacks on an installed
// system, the dev daemon's own state directory otherwise.
const stacksDirName = "stacks"

// wireStacks is what main.go calls to make the Compose stack model
// reachable: the /stacks operations (Handler.Stacks). stateDir is the
// daemon's state directory, so stacks never land anywhere else. Up, and
// Remove with deleteAppdata, take the same array check and hold as a
// container start and an appdata deletion, and the appdata roots and Engine
// listing the container remove uses; with no Docker service (apps nil) they
// are refused, since the array state is unreadable there. Every Remove lists
// the project's containers before `compose down`, so with no Docker service
// it is refused too.
func wireStacks(handler *api.Handler, stacks *store.StackStore, cipher container.SecretCipher, runner container.Runner, stateDir string, apps *appServices, admit func() (func(), error)) {
	svc := &container.StackService{
		Store:  stacks,
		Cipher: cipher,
		Runner: runner,
		Root:   filepath.Join(stateDir, stacksDirName),
	}
	if apps != nil {
		svc.RequireArrayRunning = apps.Lifecycle.RequireArrayRunning
		svc.Admit = admit
		svc.Provider = apps.Lifecycle.Provider
		svc.AppdataRoots = apps.Lifecycle.AppdataRoots
	}
	handler.Stacks = svc
}
