package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/update"
)

const (
	handlerIndexURL = "https://hoserva.dev/releases/index.json"
	handlerDebURL   = "https://github.com/mdg-labs/hoserva/releases/download/v0.2.0/hoserva_0.2.0_amd64.deb"
)

func newUpdateHandler(t *testing.T, deb []byte) (*api.Handler, *update.FakeInstaller, *update.FakeNotifier, *update.FakeJobs) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(deb)
	hexSum := hex.EncodeToString(sum[:])
	sums := []byte(hexSum + "  hoserva_0.2.0_amd64.deb\n")
	sig := ed25519.Sign(priv, sums)
	idx, err := json.Marshal(update.Index{Channels: map[update.Channel][]update.Release{
		update.ChannelStable: {{
			Tag:     "v0.2.0",
			Version: "0.2.0",
			Channel: update.ChannelStable,
			Assets: map[string]update.Asset{
				"amd64": {URL: handlerDebURL, SHA256: hexSum},
				"arm64": {URL: strings.Replace(handlerDebURL, "amd64", "arm64", 1), SHA256: hexSum},
			},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	sumsURL := strings.TrimSuffix(handlerDebURL, "hoserva_0.2.0_amd64.deb") + "SHA256SUMS"
	fetcher := &update.MapFetcher{Bodies: map[string][]byte{
		handlerIndexURL:  idx,
		handlerDebURL:    deb,
		sumsURL:          sums,
		sumsURL + ".sig": sig,
	}}
	inst := &update.FakeInstaller{}
	notes := &update.FakeNotifier{}
	jobs := &update.FakeJobs{}
	eng := &update.Engine{
		IndexURL:  handlerIndexURL,
		Arch:      "amd64",
		StateDir:  t.TempDir(),
		Current:   "0.1.0",
		PublicKey: pub,
		Fetcher:   fetcher,
		Installer: inst,
		Host:      &update.FakeHost{Versions: map[string]string{"mergerfs": "2.40.2", "snapraid": "12.4"}},
		Jobs:      jobs,
		Backup:    &update.FakeBackup{},
		Notify:    notes,
		Settings:  update.DefaultMemorySettings(),
	}
	return &api.Handler{Updates: eng}, inst, notes, jobs
}

func TestHandlerApplyUpdateRequiresConfirm(t *testing.T) {
	h, inst, _, _ := newUpdateHandler(t, []byte("deb"))
	_, err := h.ApplyUpdate(context.Background(), &apiv1.ConfirmUpdateRequest{Confirm: false})
	if err == nil {
		t.Fatal("ApplyUpdate without confirm succeeded")
	}
	if !strings.Contains(err.Error(), "confirmation") {
		t.Fatalf("err = %v, want confirmation_required", err)
	}
	if len(inst.Calls) != 0 {
		t.Fatalf("installer called: %v", inst.Calls)
	}
}

func TestHandlerApplyUpdateRefusesNamedStorageJob(t *testing.T) {
	h, inst, _, jobs := newUpdateHandler(t, []byte("deb"))
	jobs.Blocking = &job.Job{ID: "11111111-1111-4111-8111-111111111111", Type: job.TypeSync, Class: job.ClassParity}
	_, err := h.ApplyUpdate(context.Background(), &apiv1.ConfirmUpdateRequest{Confirm: true})
	if err == nil {
		t.Fatal("ApplyUpdate succeeded while a sync was running")
	}
	if !strings.Contains(err.Error(), jobs.Blocking.ID) || !strings.Contains(err.Error(), string(job.ClassParity)) {
		t.Fatalf("refusal %q does not name the blocking job", err)
	}
	if len(inst.Calls) != 0 {
		t.Fatalf("installer called: %v", inst.Calls)
	}
}

func TestHandlerApplyUpdateChecksumMismatchInstallsNothing(t *testing.T) {
	h, inst, notes, _ := newUpdateHandler(t, []byte("good-deb"))
	orig, ok := h.Updates.Fetcher.(*update.MapFetcher)
	if !ok {
		t.Fatal("expected MapFetcher")
	}
	orig.Bodies[handlerDebURL] = []byte("tampered")
	_, err := h.ApplyUpdate(context.Background(), &apiv1.ConfirmUpdateRequest{Confirm: true})
	if err == nil {
		t.Fatal("ApplyUpdate installed a tampered package")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
	if len(inst.Calls) != 0 {
		t.Fatalf("installer called after checksum failure: %v", inst.Calls)
	}
	notified := false
	for _, title := range notes.Titles {
		if strings.Contains(strings.ToLower(title), "not installed") {
			notified = true
			break
		}
	}
	if !notified {
		t.Fatalf("failed checksum did not notify: %v", notes.Titles)
	}
}
