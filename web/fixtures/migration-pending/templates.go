// Package migrationpending embeds the Unraid templates and the verify results of
// the migration-pending scenario (doc 06 §8): one template that converts cleanly
// and one that converts with warnings, and a passing and a failing result of the
// verify phase. cmd/mockapi converts the templates with the production converter,
// so the preview the mock serves is what the daemon would show, and serves the
// verify results as getMigration's `verify`.
package migrationpending

import "embed"

// Templates holds the scenario's templates, one XML file each.
//
//go:embed *.xml
var Templates embed.FS

// Verify holds the scenario's verify results, each a MigrationVerify document:
// verify-passed.json, where every disk and share matches the baseline, and
// verify-failed.json, where a file is missing, one is truncated and one has a
// changed checksum.
//
//go:embed verify-*.json
var Verify embed.FS
