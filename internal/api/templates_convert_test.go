package api_test

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

const convertTemplateXML = `<?xml version="1.0"?>
<Container version="2">
  <Name>files</Name>
  <Repository>example.com/files:2.0</Repository>
  <Network>br0</Network>
  <Overview>Serves files.</Overview>
  <ExtraParams>--cap-add=NET_ADMIN --exotic-flag=1</ExtraParams>
  <Config Name="Data" Target="/data" Mode="rw" Type="Path">/mnt/disks/usb/data</Config>
  <Config Name="Token" Target="TOKEN" Description="API token" Mask="true" Type="Variable">abc</Config>
</Container>`

func TestConvertUnraidTemplate_ReturnsComposeBesideTheSourceWithEveryWarning(t *testing.T) {
	h, _, _ := newTestHandler(t)
	got, err := h.ConvertUnraidTemplate(context.Background(), &apiv1.UnraidConvertRequest{XML: convertTemplateXML})
	if err != nil {
		t.Fatalf("ConvertUnraidTemplate: %v", err)
	}
	if got.Source != convertTemplateXML {
		t.Error("the response does not carry the source XML as sent")
	}
	if !strings.Contains(got.Compose, "image: example.com/files:2.0") {
		t.Errorf("compose = %q", got.Compose)
	}
	if got.Clean {
		t.Error("a template with an untranslated flag, a flagged path and a custom network is reported clean")
	}
	classes := map[apiv1.ConversionWarningClass]apiv1.ConversionWarning{}
	for _, w := range got.Warnings {
		classes[w.Class] = w
	}
	for _, c := range []apiv1.ConversionWarningClass{
		apiv1.ConversionWarningClassUntranslatedFlag, apiv1.ConversionWarningClassFlaggedPath,
		apiv1.ConversionWarningClassMissingNetwork, apiv1.ConversionWarningClassWritableLayer,
	} {
		if _, ok := classes[c]; !ok {
			t.Errorf("no %s warning in %+v", c, got.Warnings)
		}
	}
	if cmd := classes[apiv1.ConversionWarningClassMissingNetwork].Command.Or(""); !strings.HasPrefix(cmd, "docker network create ") || !strings.HasSuffix(cmd, " br0") {
		t.Errorf("network command = %q", cmd)
	}
	if d := classes[apiv1.ConversionWarningClassUntranslatedFlag].Detail.Or(""); d != "--exotic-flag=1" {
		t.Errorf("untranslated flag detail = %q", d)
	}
	kinds := map[apiv1.TemplatePrivilegeKind]bool{}
	for _, p := range got.Privileges {
		kinds[p.Kind] = true
	}
	if !kinds[apiv1.TemplatePrivilegeKindAddedCapabilities] || !kinds[apiv1.TemplatePrivilegeKindHostPath] {
		t.Errorf("privileges = %+v, want added capabilities and a host path", got.Privileges)
	}
	if got.Metadata.Title != "files" || got.Metadata.Overview.Or("") != "Serves files." {
		t.Errorf("metadata = %+v", got.Metadata)
	}
	if len(got.Metadata.Variables) != 1 || !got.Metadata.Variables[0].Secret || got.Metadata.Variables[0].Description.Or("") != "API token" {
		t.Errorf("variables = %+v", got.Metadata.Variables)
	}
}

func TestConvertUnraidTemplate_RefusesWhatIsNotATemplateWith400(t *testing.T) {
	h, _, _ := newTestHandler(t)
	for _, xml := range []string{"not xml", "<Compose/>", "<Container><Name>x</Name></Container>"} {
		_, err := h.ConvertUnraidTemplate(context.Background(), &apiv1.UnraidConvertRequest{XML: xml})
		if status, code := statusOf(h, err); status != 400 || code != "invalid_unraid_template" {
			t.Errorf("ConvertUnraidTemplate(%q) = %d %q, want 400 invalid_unraid_template", xml, status, code)
		}
	}
}
