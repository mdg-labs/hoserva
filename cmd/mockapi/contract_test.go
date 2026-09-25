package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// contractCase is one row of the #272 contract table: run sends the same
// sequence of calls to both handlers (any setup a case needs is part of
// run itself, since a created id is generated independently — and
// differently — by each handler's own uuid.New(), so it can only be
// captured from the handler it was just created on, never precomputed
// once and reused against the other). Only run's own returned error is
// compared: an unexpected error from a setup step still produces a
// meaningful (and diagnostic) mismatch or match, it is just attributed to
// the whole case rather than to one call inside it.
//
// op names the apiv1.Handler method this case's own comparison exercises
// — the one assertContractCoverage checks off against doc 06 §8's
// coverage rule: every operation needs a table entry or a reviewed
// contractSkip entry, or the test fails. scenario selects the mockapi
// fixture (and the matching production rig topology, see
// contract_rig_test.go) the case runs against; "" defaults to "healthy".
type contractCase struct {
	op       string
	name     string
	scenario string
	run      func(ctx context.Context, h apiv1.Handler) error
}

// contractOutcome reduces a handler call's own result to the (status,
// code) pair api/openapi.yaml's shared Error schema carries (D18) — nil
// for success, matching contractCase.run's own "return the last error"
// contract.
func contractOutcome(h apiv1.Handler, err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	status := h.NewError(context.Background(), err)
	return status.StatusCode, status.Response.Code
}

func runContractCase(t *testing.T, tc contractCase) {
	t.Helper()
	scenario := tc.scenario
	if scenario == "" {
		scenario = "healthy"
	}
	prod, mock := newContractRig(t, scenario)
	ctx := context.Background()

	prodErr := tc.run(ctx, prod)
	mockErr := tc.run(ctx, mock)
	// A case named "valid…" (bullet 1 of #272's acceptance: "at least one
	// valid request") asserts success outright, not just agreement —
	// otherwise the same defect breaking both handlers the same way would
	// pass silently.
	if strings.HasPrefix(tc.name, "valid") {
		if prodErr != nil {
			t.Fatalf("production: %v", prodErr)
		}
		if mockErr != nil {
			t.Fatalf("mock: %v", mockErr)
		}
	}

	prodStatus, prodCode := contractOutcome(prod, prodErr)
	mockStatus, mockCode := contractOutcome(mock, mockErr)
	if prodStatus != mockStatus || prodCode != mockCode {
		t.Fatalf("production = (%d %q), mock = (%d %q) — the mock must match production's status and error code (D18)",
			prodStatus, prodCode, mockStatus, mockCode)
	}
}

// TestContract_MockMatchesProductionValidation is #272's contract test:
// cmd/mockapi and internal/api's real handler (built against fakes, never
// the mock's own fixtures) run the same request table, and any
// difference in HTTP status or error code fails the test. Response-body
// equality and mock scenario fixtures are out of scope (#272) — the
// generated server interfaces already enforce response shape (D18).
func TestContract_MockMatchesProductionValidation(t *testing.T) {
	covered := map[string]bool{}
	hasValid := map[string]bool{}
	for _, tc := range contractCases {
		tc := tc
		covered[tc.op] = true
		if strings.HasPrefix(tc.name, "valid") {
			hasValid[tc.op] = true
		}
		t.Run(tc.op+"/"+tc.name, func(t *testing.T) {
			runContractCase(t, tc)
		})
	}
	assertContractCoverage(t, covered)
	assertContractNoValidCase(t, covered, hasValid)
}

// assertContractCoverage fails for any apiv1.Handler operation that is
// neither in contractCases nor contractSkip: a new operation with no
// table entry fails this test until one of the two is added (#272's own
// acceptance criterion), so API surface can never silently outrun this
// contract.
func assertContractCoverage(t *testing.T, covered map[string]bool) {
	t.Helper()
	handlerType := reflect.TypeOf((*apiv1.Handler)(nil)).Elem()
	for i := 0; i < handlerType.NumMethod(); i++ {
		name := handlerType.Method(i).Name
		if name == "NewError" {
			continue
		}
		if covered[name] {
			continue
		}
		reason, skipped := contractSkip[name]
		if !skipped {
			t.Errorf("%s has no contract-test case in contractCases and no entry in contractSkip — add one before this lands (#272)", name)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("contractSkip[%q] has an empty reason", name)
		}
	}
}

// assertContractNoValidCase keeps contractNoValidCase honest in both
// directions: every op that has a table entry but no case whose name
// starts with "valid" must be named here with a reason (so a future case
// silently dropping its only valid variant fails this test, not just
// eyeballing a diff), and every key here must still name a real op that
// still has no valid case (so a fixed op's entry is deleted, not left to
// rot).
func assertContractNoValidCase(t *testing.T, covered, hasValid map[string]bool) {
	t.Helper()
	for op := range covered {
		if hasValid[op] {
			continue
		}
		reason, listed := contractNoValidCase[op]
		if !listed {
			t.Errorf("%s has a contract-test case but none named valid…, and no entry in contractNoValidCase explaining why (#272)", op)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("contractNoValidCase[%q] has an empty reason", op)
		}
	}
	for op := range contractNoValidCase {
		if !covered[op] {
			t.Errorf("contractNoValidCase[%q] names an operation with no contractCases entry at all", op)
			continue
		}
		if hasValid[op] {
			t.Errorf("contractNoValidCase[%q] is stale — this op now has a case named valid… — remove the entry", op)
		}
	}
}

// contractSkip is #272's own explicit, reviewed skip list: every
// apiv1.Handler operation with no contractCases entry must be named
// here, with a reason, or assertContractCoverage fails. The rig wires a
// fake for every operation internal/api's own *_handler_test.go files
// build one for — schedules, settings, UPS, network/ACME/TLS, external
// disks, host-config/doctor, metrics, wake events, the update engine,
// sessions and share permissions are all covered by contractCases
// entries instead. What remains is six operations, each with a specific
// reason no table entry can compare production against the mock without
// encoding a difference the mock carries by design, or that no fake
// anywhere in the repo — not even production's own tests — reaches.
var contractSkip = map[string]string{
	// The mock's own session layer treats every request as already
	// authenticated by a single canned admin, with any credential
	// accepted for Login and no session/principal state checked for
	// these four (cmd/mockapi/auth.go's own comment on mockSessionCookie:
	// "the mock never validates a session"). Production's own handler-
	// level tests (auth_handler_test.go) reach these only by first
	// synthesizing a request-scoped principal through
	// SessionSecurityHandler.HandleSessionCookie — the generated
	// server's security dispatch, which this rig (like that test file)
	// deliberately bypasses to test Handler methods directly. Passing a
	// synthesized principal context to the mock changes nothing there —
	// it ignores ctx for these four entirely — so the only signal a case
	// could add is the one genuinely different behaviour (Login accepts
	// any password; GetCurrentSession/Logout/EnrollTotp/ConfirmTotp
	// never check for a principal at all), which is the mock's own
	// documented simplification for local UI dev, not drift — see doc 06
	// §8: any credential accepted for Login, no session ever validated
	// for any request.
	"Login":             "the mock accepts any credential by design (cmd/mockapi/auth.go) — production's Login checks the real password; comparing would encode that documented simplification as a bug, not drift",
	"Logout":            "the mock has no session/principal concept for any request — GetCurrentSession/Logout/EnrollTotp/ConfirmTotp all return canned success regardless of ctx; production needs a principal HandleSessionCookie would inject",
	"GetCurrentSession": "same as Logout — the mock returns mockUser() unconditionally; production requires a request-scoped principal",
	"EnrollTotp":        "same as Logout — the mock has no real TOTP secret or session state to enrol against",
	"ConfirmTotp":       "same as Logout — the mock has no real TOTP secret or session state to confirm against",

	// ExportConfig: no test anywhere in internal/api (no
	// backup_handler_test.go, no coverage in phase1_handler_test.go)
	// exercises Handler.ExportConfig or Handler.ImportConfig at the
	// handler level — production's own suite never reaches this
	// operation with a fake either. ImportConfig's own missing-confirm
	// case (contract_cases_test.go) needs no backup.Service wiring at
	// all, since ImportConfig checks Confirm before touching h.Backup,
	// but ExportConfig has no such early-exit: reaching it requires a
	// fully wired backup.Service (a real DB path, Paths, SecretSource,
	// Cipher — internal/backup has none of these as a scriptable fake),
	// and the mock always returns canned archive bytes with no failure
	// path to compare against.
	"ExportConfig": "no internal/api test exercises this handler either; backup.Service needs a real DB path, Paths and SecretSource this pass does not build, and the mock has no failure path to compare against",
}

// contractNoValidCase is #272's own accounting of every operation this
// pass could not give a valid (both-sides-succeed) case despite having a
// contractCases entry: each is still exercised by at least one failure
// case above, comparing status and error code the ordinary way — only
// the "at least one valid request" half of #272's acceptance criterion is
// unmet, and each entry says why. This is reviewed alongside
// contractSkip, never edited to make a case pass instead of reaching a
// real fix.
var contractNoValidCase = map[string]string{
	// FinishDiskRemoval only ever succeeds on a data disk store.
	// ArrayDisk.RemovalState has already reached "evacuated" — set by
	// job.RunEvacuation on a completed evacuation run, never by
	// Scheduler.Submit's own admission checks this pass contract-tests.
	// This rig registers TypeEvacuation with a no-op RunFunc (job-timing/
	// run-behaviour state, out of scope per #272's own "jobs, SSE
	// timing").
	"FinishDiskRemoval": "needs an evacuation job to actually run to completion and mark the disk evacuated (job.RunEvacuation); this rig's job types run no-op (job-timing state, out of scope)",

	// GetJobLog's own log file is created by Scheduler.runJob, which
	// starts in its own goroutine after Submit returns (scheduler.go) —
	// no synchronous call in this rig's own request/response shape
	// guarantees the file exists yet, so a case here would be flaky by
	// construction rather than a genuine validation comparison.
	"GetJobLog": "the job's log file is written asynchronously once Scheduler.runJob's own goroutine starts (job-timing state, out of scope)",

	// CancelJob: a sync job the rig just queued gives production 200
	// and the mock 409 job_not_cancellable — the rig's own registration
	// (contractProductionRunFuncs) marks every job type cancellable,
	// unlike hoservad's own registration of job.TypeSync as not
	// cancellable (cmd/hoservad/main.go). That mismatch, and whether a
	// case succeeds at all, are both job-timing/run-registration state
	// (#272's own out-of-scope list), so no CancelJob valid case is
	// added here.
	"CancelJob": "a just-queued sync job is cancellable in this rig's production registration (contractProductionRunFuncs registers every job type cancellable) but not in the mock's (StartSync submits with cancellable=false, phase1.go) — job-timing/run-registration state either way, out of scope (#272)",

	// ResumeJob only ever succeeds against an interrupted, resumable
	// job — reachable only by catching a running TypeDiskUpgradeData
	// job mid-flight when maintenance mode forces it to checkpoint,
	// again job-timing state this rig's no-op RunFuncs never produce.
	"ResumeJob": "needs an interrupted resumable job (maintenance mode's own forced-checkpoint timing, out of scope)",

	// RevokeSession: Login/CreateFirstAdmin's own canned mock cookie
	// (auth.go's mockSessionCookie) carries no corresponding h.sessions
	// entry — the mock "never validates a session" by design (the same
	// documented simplification contractSkip's Login/Logout entries
	// already cover) — so no API call on the mock side ever produces a
	// session id RevokeSession could revoke.
	"RevokeSession": "the mock never creates real session state for Login (cmd/mockapi/auth.go) — no API call gives it a session id to revoke",

	// ImportConfig: contractSkip's own ExportConfig entry explains why —
	// ImportConfig's missing-confirm case needs no backup.Service at
	// all, but a genuine import needs one fully wired (a real DB path,
	// Paths, SecretSource, Cipher), which this rig does not build, and
	// h.Backup is nil here.
	"ImportConfig": "a real import needs a fully wired backup.Service (DB path, Paths, SecretSource, Cipher) this rig does not build — h.Backup is nil here, same as ExportConfig",

	// The three root-only recovery operations (doc 01 §7): cmd/mockapi's
	// own recovery.go refuses ResetUserPassword/DisableUserTotp/
	// UnlockUser unconditionally, by design — it carries no peer-
	// credential layer at all (internal/auth.PeerCredentialFromContext),
	// unlike production's own requireRootPeer, which a test can satisfy
	// with a root peer credential (auth.WithPeerCredential(ctx,
	// PeerCredential{UID: 0}), internal/api/recovery_handler_test.go).
	// This rig's own calls carry no such credential either, so no case
	// here can make the mock succeed.
	"ResetUserPassword": "the mock refuses this operation unconditionally by design (cmd/mockapi/recovery.go has no peer-credential layer) — production itself is reachable with a root peer credential in tests (auth.WithPeerCredential, internal/api/recovery_handler_test.go), but the mock never succeeds regardless",
	"DisableUserTotp":   "same as ResetUserPassword — the mock refuses this operation unconditionally by design",
	"UnlockUser":        "same as ResetUserPassword — the mock refuses this operation unconditionally by design",
}
