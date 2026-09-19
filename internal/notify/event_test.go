package notify

import "testing"

func TestEveryCatalogEventHasADefaultSeverity(t *testing.T) {
	for _, event := range EventCatalog {
		severity, ok := DefaultSeverity(event)
		if !ok {
			t.Errorf("event %s has no compiled-in default severity", event)
			continue
		}
		if !ValidSeverity(severity) {
			t.Errorf("event %s default severity %q is not a valid Severity", event, severity)
		}
	}
}

func TestValidEventType(t *testing.T) {
	if !ValidEventType(EventDiskOffline) {
		t.Error("ValidEventType(EventDiskOffline) = false, want true")
	}
	if ValidEventType(EventType("not_a_real_event")) {
		t.Error("ValidEventType(bogus) = true, want false")
	}
	if ValidEventType(EventTest) {
		t.Error("ValidEventType(EventTest) = true, want false — EventTest is not part of the routable catalog")
	}
}

func TestValidChannelType(t *testing.T) {
	for _, ct := range []ChannelType{ChannelEmail, ChannelGotify, ChannelNtfy, ChannelDiscord, ChannelWebhook} {
		if !ValidChannelType(ct) {
			t.Errorf("ValidChannelType(%s) = false, want true", ct)
		}
	}
	if ValidChannelType(ChannelType("sms")) {
		t.Error("ValidChannelType(sms) = true, want false")
	}
}
