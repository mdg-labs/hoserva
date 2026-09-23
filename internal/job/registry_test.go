package job

import (
	"context"
	"testing"
)

func TestRegistry_RegisterAbort_PanicsForUnregisteredType(t *testing.T) {
	r := NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("RegisterAbort(unregistered type) did not panic")
		}
	}()
	r.RegisterAbort(TypeDiskUpgradeData, func(context.Context, []byte) error { return nil })
}

func TestRegistry_RegisterAbort_PanicsOnDoubleRegistration(t *testing.T) {
	r := NewRegistry()
	r.Register(TypeDiskUpgradeData, true, func(context.Context, *RunContext) error { return nil })
	r.RegisterAbort(TypeDiskUpgradeData, func(context.Context, []byte) error { return nil })
	defer func() {
		if recover() == nil {
			t.Fatal("RegisterAbort called twice for the same type did not panic")
		}
	}()
	r.RegisterAbort(TypeDiskUpgradeData, func(context.Context, []byte) error { return nil })
}

func TestRegistry_LookupAbort_FalseWhenNoneRegistered(t *testing.T) {
	r := NewRegistry()
	r.Register(TypeMover, true, func(context.Context, *RunContext) error { return nil })
	if _, ok := r.lookupAbort(TypeMover); ok {
		t.Fatal("lookupAbort(TypeMover) = true, want false — nothing registered an abort func")
	}
}

func TestRegistry_LookupAbort_ReturnsRegisteredFunc(t *testing.T) {
	r := NewRegistry()
	r.Register(TypeDiskUpgradeData, true, func(context.Context, *RunContext) error { return nil })
	called := false
	r.RegisterAbort(TypeDiskUpgradeData, func(context.Context, []byte) error {
		called = true
		return nil
	})
	abort, ok := r.lookupAbort(TypeDiskUpgradeData)
	if !ok {
		t.Fatal("lookupAbort(TypeDiskUpgradeData) = false, want true")
	}
	if err := abort(context.Background(), nil); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if !called {
		t.Fatal("the registered abort func was never called")
	}
}
