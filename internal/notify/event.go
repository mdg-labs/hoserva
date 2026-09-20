package notify

// Severity mirrors api/openapi.yaml's NotificationLevel (doc 01 §5): the
// severity a routed event carries, and the one thing quiet hours ever
// checks (Severity == Critical) for its undisable override.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// EventType is one of doc 03 §8.3's fixed catalog. Values match
// api/openapi.yaml's NotificationEventType exactly — internal/api's
// handler only ever converts between the two, never re-derives the set.
type EventType string

const (
	EventSmartWarning             EventType = "smart_warning"
	EventSmartFailure             EventType = "smart_failure"
	EventDiskOffline              EventType = "disk_offline"
	EventArrayDegraded            EventType = "array_degraded"
	EventSyncSucceeded            EventType = "sync_succeeded"
	EventSyncFailed               EventType = "sync_failed"
	EventSyncBlockedThreshold     EventType = "sync_blocked_threshold"
	EventScrubErrorsFound         EventType = "scrub_errors_found"
	EventPoolAboveThreshold       EventType = "pool_above_threshold"
	EventDiskNearMinFreeSpace     EventType = "disk_near_minfreespace"
	EventCacheAboveThreshold      EventType = "cache_above_threshold"
	EventMoverSkippingFiles       EventType = "mover_skipping_files"
	EventConfigDriftDetected      EventType = "config_drift_detected"
	EventContainerUnhealthy       EventType = "container_unhealthy"
	EventContainerUpdateAvailable EventType = "container_update_available"
	EventHoservaUpdateAvailable   EventType = "hoserva_update_available"
	EventHoservaUpdateFailed      EventType = "hoserva_update_failed"
	EventRebootRequired           EventType = "reboot_required"
	EventUPSOnBattery             EventType = "ups_on_battery"
	EventUPSBatteryLow            EventType = "ups_battery_low"
	EventLoginFailureBurst        EventType = "login_failure_burst"
	EventCredentialReset          EventType = "credential_reset"
	EventCertificateExpiring      EventType = "certificate_expiring"
	EventCertificateRenewalFailed EventType = "certificate_renewal_failed"
	EventConfigBackupFailed       EventType = "config_backup_failed"
	EventAppdataBackupFailed      EventType = "appdata_backup_failed"
	EventBackupDestinationStale   EventType = "backup_destination_stale"
	EventRestoreDrillFailed       EventType = "restore_drill_failed"
)

// EventTest is not part of EventCatalog and is never routed or shown in
// the routing matrix — sendTestNotification (doc 03 §8.3) and this
// package's own tests use it to carry a synthetic Message through
// Service.Publish/Service.TestChannel without it ever being mistaken for
// a real, user-routable event.
const EventTest EventType = "test"

// EventCatalog is every event type doc 03 §8.3 lists, in that doc's own
// order — GetNotificationRouting (doc 03 §8.3's routing matrix) walks this
// slice so every event type appears even before it has ever been routed
// or had its severity overridden.
var EventCatalog = []EventType{
	EventSmartWarning,
	EventSmartFailure,
	EventDiskOffline,
	EventArrayDegraded,
	EventSyncSucceeded,
	EventSyncFailed,
	EventSyncBlockedThreshold,
	EventScrubErrorsFound,
	EventPoolAboveThreshold,
	EventDiskNearMinFreeSpace,
	EventCacheAboveThreshold,
	EventMoverSkippingFiles,
	EventConfigDriftDetected,
	EventContainerUnhealthy,
	EventContainerUpdateAvailable,
	EventHoservaUpdateAvailable,
	EventHoservaUpdateFailed,
	EventRebootRequired,
	EventUPSOnBattery,
	EventUPSBatteryLow,
	EventLoginFailureBurst,
	EventCredentialReset,
	EventCertificateExpiring,
	EventCertificateRenewalFailed,
	EventConfigBackupFailed,
	EventAppdataBackupFailed,
	EventBackupDestinationStale,
	EventRestoreDrillFailed,
}

// defaultSeverity is every event type's compiled-in default severity — the
// value GetEventSeverity returns absent a notify_event_severity override.
// sync_succeeded is deliberately low severity (info) and, separately,
// off by default: nothing routes to it until a user opts in (doc 03 §8.3),
// exactly like every other event type with no configured routes yet —
// there is nothing special in this map for "off by default", since an
// empty routing table already means that for all of them.
var defaultSeverity = map[EventType]Severity{
	EventSmartWarning:             SeverityWarning,
	EventSmartFailure:             SeverityCritical,
	EventDiskOffline:              SeverityCritical,
	EventArrayDegraded:            SeverityCritical,
	EventSyncSucceeded:            SeverityInfo,
	EventSyncFailed:               SeverityError,
	EventSyncBlockedThreshold:     SeverityCritical,
	EventScrubErrorsFound:         SeverityError,
	EventPoolAboveThreshold:       SeverityWarning,
	EventDiskNearMinFreeSpace:     SeverityWarning,
	EventCacheAboveThreshold:      SeverityWarning,
	EventMoverSkippingFiles:       SeverityWarning,
	EventConfigDriftDetected:      SeverityWarning,
	EventContainerUnhealthy:       SeverityError,
	EventContainerUpdateAvailable: SeverityInfo,
	EventHoservaUpdateAvailable:   SeverityInfo,
	EventHoservaUpdateFailed:      SeverityError,
	EventRebootRequired:           SeverityWarning,
	EventUPSOnBattery:             SeverityWarning,
	EventUPSBatteryLow:            SeverityCritical,
	EventLoginFailureBurst:        SeverityCritical,
	EventCredentialReset:          SeverityCritical,
	EventCertificateExpiring:      SeverityWarning,
	EventCertificateRenewalFailed: SeverityError,
	EventConfigBackupFailed:       SeverityError,
	EventAppdataBackupFailed:      SeverityError,
	EventBackupDestinationStale:   SeverityWarning,
	EventRestoreDrillFailed:       SeverityError,
}

// DefaultSeverity returns event's compiled-in default severity. Every
// EventCatalog member has one; an event type outside the catalog (never
// constructed by this package or the generated API types) returns "" and
// false.
func DefaultSeverity(event EventType) (Severity, bool) {
	s, ok := defaultSeverity[event]
	return s, ok
}

// ValidEventType reports whether event is a member of EventCatalog.
func ValidEventType(event EventType) bool {
	_, ok := defaultSeverity[event]
	return ok
}

// ValidSeverity reports whether s is one of the four NotificationLevel
// values.
func ValidSeverity(s Severity) bool {
	switch s {
	case SeverityInfo, SeverityWarning, SeverityError, SeverityCritical:
		return true
	default:
		return false
	}
}
