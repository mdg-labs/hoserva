package api_test

import (
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/web/fixtures"
)

// The mock server (cmd/mockapi) and hoservad's own handler are supposed to
// converge on the same fixtures (doc 06 §8, doc 12 §2: "fixtures are
// shared with backend tests"). internal/api's real handler is still the
// stub in handler.go (#19 wires it to the job system and store) so there
// is no business logic here yet to exercise against these jobs — but this
// package already imports and decodes the exact files cmd/mockapi serves,
// so the day #19 lands real ListJobs/GetJob logic, both packages are
// already reading from, and breaking against, one shared source.
func TestSharedFixturesDecodeForBackendUse(t *testing.T) {
	for _, scenario := range fixtures.Scenarios {
		raw, err := fixtures.JobsJSON(scenario)
		if err != nil {
			t.Fatalf("JobsJSON(%q): %v", scenario, err)
		}

		var listJobsOK apiv1.ListJobsOK
		if err := listJobsOK.UnmarshalJSON(raw); err != nil {
			t.Fatalf("scenario %q: decode jobs.json: %v", scenario, err)
		}
		for _, job := range listJobsOK.Jobs {
			if err := job.Validate(); err != nil {
				t.Errorf("scenario %q: job %s fails Validate(): %v", scenario, job.ID, err)
			}
		}
	}
}
