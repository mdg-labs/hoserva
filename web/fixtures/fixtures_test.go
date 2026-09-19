package fixtures_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/api/gen/go/events"
	"github.com/mdg-labs/hoserva/web/fixtures"
)

// Every fixture is decoded strictly through the generated ogen types (the
// same types cmd/mockapi and api/openapi.yaml's clients use), so a fixture
// that doesn't match the spec fails here the same way a real response
// would fail a client (D18). ogen's decoder only reads fields its schema
// declares, so decode success alone would not catch a fixture carrying an
// extra, silently-ignored field; assertNoDroppedFields below re-encodes
// the decoded value and confirms every key in the original document
// survived the round trip.
func TestFixturesValidateAgainstSpec(t *testing.T) {
	if len(fixtures.Scenarios) != 6 {
		t.Fatalf("doc 06 §8 names 6 scenarios, fixtures.Scenarios has %d", len(fixtures.Scenarios))
	}

	for _, scenario := range fixtures.Scenarios {
		t.Run(scenario, func(t *testing.T) {
			raw, err := fixtures.JobsJSON(scenario)
			if err != nil {
				t.Fatalf("JobsJSON: %v", err)
			}

			var listJobsOK apiv1.ListJobsOK
			if err := listJobsOK.UnmarshalJSON(raw); err != nil {
				t.Fatalf("decode jobs.json as ListJobsOK: %v", err)
			}
			assertNoDroppedFields(t, raw, &listJobsOK)

			for i := range listJobsOK.Jobs {
				if err := listJobsOK.Jobs[i].Validate(); err != nil {
					t.Errorf("jobs[%d] (%s) fails Job.Validate(): %v", i, listJobsOK.Jobs[i].ID, err)
				}
			}

			if rawParity, err := fixtures.ParityJSON(scenario); err == nil {
				var snap apiv1.ParitySnapshot
				if err := snap.UnmarshalJSON(rawParity); err != nil {
					t.Fatalf("decode parity.json as ParitySnapshot: %v", err)
				}
				assertNoDroppedFields(t, rawParity, &snap)
				if err := snap.Validate(); err != nil {
					t.Errorf("parity.json fails ParitySnapshot.Validate(): %v", err)
				}
			}

			rawEvents, err := fixtures.EventsJSONL(scenario)
			if err != nil {
				t.Fatalf("EventsJSONL: %v", err)
			}
			for i, line := range splitNonEmptyLines(rawEvents) {
				var ev events.Event
				if err := ev.UnmarshalJSON(line); err != nil {
					t.Fatalf("events.jsonl line %d: decode as Event: %v", i, err)
				}
				assertNoDroppedFields(t, line, &ev)
			}
		})
	}

	if _, err := fixtures.JobLog(); err != nil {
		t.Fatalf("JobLog: %v", err)
	}

	if _, err := fixtures.JobsJSON("not-a-real-scenario"); err == nil {
		t.Fatal("JobsJSON: expected an error for an unknown scenario")
	}
}

type jsonRoundTripper interface {
	MarshalJSON() ([]byte, error)
}

// assertNoDroppedFields fails t if any key present in raw is missing from
// decoded's own re-encoding, at any depth — the signature of a field the
// generated type silently ignored while decoding raw.
func assertNoDroppedFields(t *testing.T, raw []byte, decoded jsonRoundTripper) {
	t.Helper()

	var original any
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatalf("unmarshal original into interface{}: %v", err)
	}

	reencoded, err := decoded.MarshalJSON()
	if err != nil {
		t.Fatalf("re-encode decoded value: %v", err)
	}
	var roundTripped any
	if err := json.Unmarshal(reencoded, &roundTripped); err != nil {
		t.Fatalf("unmarshal re-encoded value into interface{}: %v", err)
	}

	if diff := firstMissingKeyPath("$", original, roundTripped); diff != "" {
		t.Errorf("field silently dropped decoding through the generated type: %s", diff)
	}
}

// firstMissingKeyPath walks original and roundTripped together, returning a
// JSON-pointer-ish path to the first key present in original but absent
// from roundTripped, or "" if none is missing.
func firstMissingKeyPath(path string, original, roundTripped any) string {
	switch orig := original.(type) {
	case map[string]any:
		rt, ok := roundTripped.(map[string]any)
		if !ok {
			return path
		}
		for k, v := range orig {
			rv, ok := rt[k]
			if !ok {
				return fmt.Sprintf("%s.%s", path, k)
			}
			if diff := firstMissingKeyPath(fmt.Sprintf("%s.%s", path, k), v, rv); diff != "" {
				return diff
			}
		}
		return ""
	case []any:
		rt, ok := roundTripped.([]any)
		if !ok || len(rt) != len(orig) {
			return fmt.Sprintf("%s (array length changed)", path)
		}
		for i, v := range orig {
			if diff := firstMissingKeyPath(fmt.Sprintf("%s[%d]", path, i), v, rt[i]); diff != "" {
				return diff
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(original, roundTripped) {
			// A scalar changing value (not just going missing) isn't this
			// check's concern — Validate() and the fixture's own review
			// cover value correctness. Presence is all that's asserted here.
			return ""
		}
		return ""
	}
}

func splitNonEmptyLines(b []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				lines = append(lines, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		lines = append(lines, b[start:])
	}
	return lines
}
