// Package fixtures embeds the scenario fixtures shared between cmd/mockapi
// and backend tests (doc 06 §8, doc 12 §2): both read the same files, so a
// change to one cannot silently drift from the other. It knows nothing
// about the generated API types on purpose — decoding and validating a
// fixture against api/openapi.yaml is left to its callers (mockapi and the
// tests alongside this package), so this package stays a plain file
// loader, importable from anywhere without pulling in api/gen/go.
package fixtures

import (
	"embed"
	"fmt"
)

//go:embed */jobs.json */events.jsonl common/job-log.txt
var files embed.FS

// Scenarios are the mock server's scenarios (doc 06 §8).
var Scenarios = []string{
	"healthy",
	"degraded",
	"rebuilding",
	"sync-blocked",
	"fresh-install",
	"migration-pending",
}

// Valid reports whether name is one of Scenarios.
func Valid(name string) bool {
	for _, s := range Scenarios {
		if s == name {
			return true
		}
	}
	return false
}

// JobsJSON returns scenario's jobs.json fixture: a ListJobsOK-shaped
// document (`{"jobs": [...]}`) validated against api/openapi.yaml by
// fixtures_test.go.
func JobsJSON(scenario string) ([]byte, error) {
	if !Valid(scenario) {
		return nil, fmt.Errorf("fixtures: unknown scenario %q", scenario)
	}
	return files.ReadFile(scenario + "/jobs.json")
}

// EventsJSONL returns scenario's events.jsonl fixture: one `Event`-shaped
// JSON document per line, in emission order, for the optional
// /api/v1/events replay (doc 01 §5, Q63).
func EventsJSONL(scenario string) ([]byte, error) {
	if !Valid(scenario) {
		return nil, fmt.Errorf("fixtures: unknown scenario %q", scenario)
	}
	return files.ReadFile(scenario + "/events.jsonl")
}

// JobLog is the canned job-log fixture served for every job's
// /jobs/{jobId}/log (doc 01 §5). Its content does not vary by scenario or
// job: the spec models the log as an opaque compressed byte stream, not a
// JSON schema, so there is nothing scenario-specific to validate here.
func JobLog() ([]byte, error) {
	return files.ReadFile("common/job-log.txt")
}
