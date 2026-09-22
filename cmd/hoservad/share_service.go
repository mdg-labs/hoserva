package main

import (
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
)

// newShareService builds the one real share.Service the HTTP handler
// reads (Handler.Shares, #261) — the same shareStore/arrayStore
// moverSharesFromStore and shareRelocationShareFromStore already read
// for the mover and relocation jobs, so all three read the same
// persisted state without duplicating it. mounter and usages are taken
// as share.Mounter/share.UsageReader so a test can fake the one
// dependency (Mounter) that would otherwise mount real mergerfs units
// under /mnt.
func newShareService(shareStore *store.ShareStore, arrayStore *store.ArrayStore, generator *cfggen.Generator, mounter share.Mounter, usages share.UsageReader) *share.Service {
	return &share.Service{
		Shares:  shareStore,
		Array:   arrayStore,
		Gen:     generator,
		FS:      share.OSFS{},
		Mounter: mounter,
		Usages:  usages,
	}
}
