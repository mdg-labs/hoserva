package notify

import "time"

// Alert is one in-app notification row (doc 03 §2, #188): what the
// top-bar bell lists and what Service.Publish persists before it ever
// queues external channel deliveries.
type Alert struct {
	ID        string
	EventType EventType
	Severity  Severity
	Title     string
	Message   string
	CreatedAt time.Time
	ReadAt    *time.Time
}

// AlertGroup is alerts sharing one event type, in list order.
type AlertGroup struct {
	EventType EventType
	Alerts    []Alert
}
