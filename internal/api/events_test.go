package api_test

import (
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/api/gen/go/events"
)

// The CLI's only way to read /api/v1/events is the generated events package
// (D18, doc 01 §5, Q63) — this exercises its SSE-frame Reader against one
// framed event of each type the spec's Event schema declares.
func TestEventsReaderDecodesEveryEventType(t *testing.T) {
	const stream = "" +
		"event: job_progress\n" +
		`data: {"event":"job_progress","data":{"id":"3fa85f64-5717-4562-b3fc-2c963f66afa6","type":"sync","class":"parity","status":"running","progress":42,"resumable":false,"cancellable":true,"createdAt":"2026-01-01T00:00:00Z"}}` + "\n" +
		"\n" +
		"event: disk_state\n" +
		`data: {"event":"disk_state","data":{"diskId":"disk-1","device":"/dev/sdb","state":"active","at":"2026-01-01T00:00:00Z"}}` + "\n" +
		"\n" +
		"event: container_state\n" +
		`data: {"event":"container_state","data":{"containerId":"c1","name":"jellyfin","state":"running","at":"2026-01-01T00:00:00Z"}}` + "\n" +
		"\n" +
		"event: notification\n" +
		`data: {"event":"notification","data":{"id":"n1","eventType":"pool_above_threshold","level":"warning","title":"t","message":"m","createdAt":"2026-01-01T00:00:00Z"}}` + "\n" +
		"\n"

	r := events.NewReader(strings.NewReader(stream))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("job_progress: %v", err)
	}
	if !ev.IsJobProgressEvent() {
		t.Fatalf("job_progress: got type %v", ev.Type)
	}

	ev, err = r.Next()
	if err != nil {
		t.Fatalf("disk_state: %v", err)
	}
	if !ev.IsDiskStateEvent() {
		t.Fatalf("disk_state: got type %v", ev.Type)
	}
	if got := ev.DiskStateEvent.Data.Device; got != "/dev/sdb" {
		t.Fatalf("disk_state: device = %q", got)
	}

	ev, err = r.Next()
	if err != nil {
		t.Fatalf("container_state: %v", err)
	}
	if !ev.IsContainerStateEvent() {
		t.Fatalf("container_state: got type %v", ev.Type)
	}

	ev, err = r.Next()
	if err != nil {
		t.Fatalf("notification: %v", err)
	}
	if !ev.IsNotificationEvent() {
		t.Fatalf("notification: got type %v", ev.Type)
	}

	if _, err := r.Next(); err == nil {
		t.Fatal("expected io.EOF at end of stream")
	}
}
