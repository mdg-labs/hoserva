package api

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// specPath is the repository's own api/openapi.yaml, read directly rather
// than embedded: this issue's file scope keeps internal/api independent
// of api/ (embed patterns cannot ascend into a parent directory anyway),
// so a plain file read is how this test — not production code — proves
// operationRoles hasn't drifted from the hand-written spec (D18).
const specPath = "../../api/openapi.yaml"

type specOperation struct {
	OperationID string `yaml:"operationId"`
	Role        string `yaml:"x-hoserva-role"`
}

// specOperationName mirrors ogen's own naming: the operationId with its
// first letter capitalized (confirmed against api/gen/go/oas_operations_gen.go
// for every operation this spec currently declares).
func specOperationName(operationID string) string {
	if operationID == "" {
		return ""
	}
	return strings.ToUpper(operationID[:1]) + operationID[1:]
}

// httpMethods is every key a path item's own operations can appear under
// (OpenAPI 3.1) — every other sibling key on a path item (parameters,
// summary, description, servers, $ref, ...) is not an operation and is
// skipped rather than decoded as one.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

func loadSpecRoles(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading %s: %v", specPath, err)
	}

	// The per-path value is decoded as raw yaml.Node children rather than
	// straight into map[string]specOperation: a path item's own
	// "parameters" sibling key holds a sequence, not an operation object,
	// and unmarshalling that into specOperation would fail the whole
	// parse.
	var doc struct {
		Paths map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", specPath, err)
	}

	roles := make(map[string]string)
	for path, pathItem := range doc.Paths {
		if pathItem.Kind != yaml.MappingNode {
			t.Fatalf("path %s: expected a mapping, got kind %v", path, pathItem.Kind)
		}
		for i := 0; i+1 < len(pathItem.Content); i += 2 {
			method := pathItem.Content[i].Value
			if !httpMethods[method] {
				continue
			}
			var op specOperation
			if err := pathItem.Content[i+1].Decode(&op); err != nil {
				t.Fatalf("path %s method %s: %v", path, method, err)
			}
			if op.OperationID == "" {
				t.Fatalf("path %s method %s has no operationId", path, method)
			}
			roles[specOperationName(op.OperationID)] = op.Role
		}
	}
	return roles
}

// TestOperationRolesMatchesSpec is the drift guard operationRoles' own
// comment promises: every operation api/openapi.yaml declares must have
// exactly one entry in operationRoles with the same role, streamEvents
// excluded (ogen generates no server/client for it at all — Q63 — so it
// has no apiv1.OperationName to key a map entry with).
func TestOperationRolesMatchesSpec(t *testing.T) {
	specRoles := loadSpecRoles(t)
	delete(specRoles, "StreamEvents")

	for name, wantRole := range specRoles {
		gotRole, ok := RoleFor(name)
		if !ok {
			t.Errorf("operation %s (spec role %q) has no entry in operationRoles", name, wantRole)
			continue
		}
		if string(gotRole) != wantRole {
			t.Errorf("operation %s: operationRoles has %q, spec has %q", name, gotRole, wantRole)
		}
	}

	for name := range operationRoles {
		if _, ok := specRoles[name]; !ok {
			t.Errorf("operationRoles has an entry for %s, which is not in the spec", name)
		}
	}
}

// TestOperationRolesCoversEveryGeneratedOperation guards the other
// direction of drift: a spec change that adds an operation but is never
// run through `make gen` again would leave apiv1's own operation list
// stale, which this test can't see — so it also checks operationRoles
// against every OperationName ogen actually generated, independent of the
// spec parse above.
func TestOperationRolesCoversEveryGeneratedOperation(t *testing.T) {
	generated := []apiv1.OperationName{
		apiv1.CancelJobOperation,
		apiv1.ConfirmTotpOperation,
		apiv1.CreateArrayOperation,
		apiv1.CreateFirstAdminOperation,
		apiv1.CreateNotificationChannelOperation,
		apiv1.DeleteNotificationChannelOperation,
		apiv1.DisableUserTotpOperation,
		apiv1.EnrollTotpOperation,
		apiv1.ExportConfigOperation,
		apiv1.GetCurrentSessionOperation,
		apiv1.GetGeneralSettingsOperation,
		apiv1.GetJobOperation,
		apiv1.GetJobLogOperation,
		apiv1.GetNotificationChannelOperation,
		apiv1.GetNotificationRoutingOperation,
		apiv1.GetParityOperation,
		apiv1.GetPoolOperation,
		apiv1.GetQuietHoursOperation,
		apiv1.GetSetupStatusOperation,
		apiv1.GetStatusOperation,
		apiv1.ImportConfigOperation,
		apiv1.ListDisksOperation,
		apiv1.ListWakeEventsOperation,
		apiv1.ListJobsOperation,
		apiv1.ListNotificationChannelsOperation,
		apiv1.LoginOperation,
		apiv1.LogoutOperation,
		apiv1.ResetUserPasswordOperation,
		apiv1.ResumeJobOperation,
		apiv1.RunDoctorOperation,
		apiv1.RunParityDiffOperation,
		apiv1.SendTestNotificationOperation,
		apiv1.StartArrayOperation,
		apiv1.StartFixOperation,
		apiv1.StartScrubOperation,
		apiv1.StartSyncOperation,
		apiv1.StopArrayOperation,
		apiv1.UnlockUserOperation,
		apiv1.UpdateGeneralSettingsOperation,
		apiv1.UpdateNotificationChannelOperation,
		apiv1.UpdateNotificationRouteOperation,
		apiv1.UpdateQuietHoursOperation,
	}
	sort.Slice(generated, func(i, j int) bool { return generated[i] < generated[j] })

	for _, op := range generated {
		if _, ok := RoleFor(op); !ok {
			t.Errorf("generated operation %s has no entry in operationRoles (fails closed at runtime, but should never ship that way)", op)
		}
	}
	if len(operationRoles) != len(generated) {
		t.Errorf("operationRoles has %d entries, want exactly %d (one per generated operation)", len(operationRoles), len(generated))
	}
}

func TestRoleSatisfies(t *testing.T) {
	cases := []struct {
		have, want Role
		ok         bool
	}{
		{RoleAdmin, RoleAdmin, true},
		{RoleAdmin, RoleViewer, true},
		{RoleViewer, RoleViewer, true},
		{RoleViewer, RoleAdmin, false},
	}
	for _, c := range cases {
		if got := c.have.Satisfies(c.want); got != c.ok {
			t.Errorf("Role(%q).Satisfies(%q) = %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}
