// Package migrationpending embeds the Unraid templates of the
// migration-pending scenario (doc 06 §8): one that converts cleanly and one
// that converts with warnings. cmd/mockapi converts them with the production
// converter, so the preview the mock serves is what the daemon would show.
package migrationpending

import "embed"

// Templates holds the scenario's templates, one XML file each.
//
//go:embed *.xml
var Templates embed.FS
