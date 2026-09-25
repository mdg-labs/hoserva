package main

import (
	"context"
	"errors"
	"reflect"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/parity"
)

var errNoParityEngine = errors.New("hoservad: no parity engine is wired — create the array first")

// currentParityEngine is the parity.Engine the disk replace and upgrade
// jobs are registered with. Those jobs are wired once at startup, but a
// daemon started with no array only gets its engine from a live array
// creation (parityRegistrar.register, #265), so every call resolves the
// engine Handler holds now.
type currentParityEngine struct {
	handler *api.Handler
}

func (c currentParityEngine) engine() (parity.Engine, error) {
	eng, _, _, _ := c.handler.CurrentParity()
	if eng == nil {
		return nil, errNoParityEngine
	}
	if v := reflect.ValueOf(eng); v.Kind() == reflect.Pointer && v.IsNil() {
		return nil, errNoParityEngine
	}
	return eng, nil
}

func (c currentParityEngine) Sync(ctx context.Context, opts parity.SyncOpts) (<-chan parity.Progress, error) {
	eng, err := c.engine()
	if err != nil {
		return nil, err
	}
	return eng.Sync(ctx, opts)
}

func (c currentParityEngine) Diff(ctx context.Context) (parity.DiffReport, error) {
	eng, err := c.engine()
	if err != nil {
		return parity.DiffReport{}, err
	}
	return eng.Diff(ctx)
}

func (c currentParityEngine) Scrub(ctx context.Context, pct, olderThanDays int) (<-chan parity.Progress, error) {
	eng, err := c.engine()
	if err != nil {
		return nil, err
	}
	return eng.Scrub(ctx, pct, olderThanDays)
}

func (c currentParityEngine) Status(ctx context.Context) (parity.ParityStatus, error) {
	eng, err := c.engine()
	if err != nil {
		return parity.ParityStatus{}, err
	}
	return eng.Status(ctx)
}

func (c currentParityEngine) Fix(ctx context.Context, opts parity.FixOpts) (<-chan parity.Progress, error) {
	eng, err := c.engine()
	if err != nil {
		return nil, err
	}
	return eng.Fix(ctx, opts)
}

func (c currentParityEngine) Check(ctx context.Context, opts parity.CheckOpts) (<-chan parity.Progress, error) {
	eng, err := c.engine()
	if err != nil {
		return nil, err
	}
	return eng.Check(ctx, opts)
}

func (c currentParityEngine) List(ctx context.Context) (parity.ListReport, error) {
	eng, err := c.engine()
	if err != nil {
		return parity.ListReport{}, err
	}
	return eng.List(ctx)
}
