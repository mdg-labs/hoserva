package api_test

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
)

// TestRunDoctor_WarnsForAnEngineBelowTheFloor goes through
// Handler.RunDoctor, the implementation behind GET /api/v1/doctor, with the
// handler's own Container provider reporting Engine 28.5.2.
func TestRunDoctor_WarnsForAnEngineBelowTheFloor(t *testing.T) {
	h, _ := hostConfigTestHandler(t)
	f := container.NewFakeProvider()
	f.SetVersion(container.EngineVersion{Version: "28.5.2", APIVersion: "1.51", MinAPIVersion: "1.24"})
	h.Container = f

	report, err := h.RunDoctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	check := findCheck(report, "docker")
	if check.Status != apiv1.DoctorCheckStatusWarn || !strings.Contains(check.Message, "28.5.2") {
		t.Fatalf("docker check = %+v, want a warning naming Engine 28.5.2", check)
	}
}
