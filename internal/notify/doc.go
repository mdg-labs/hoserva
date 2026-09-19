// Package notify implements Hoserva's alerting subsystem (#35, doc 03
// §8.3, doc 01 §7, Q28): five channel types (email, Gotify, ntfy, Discord
// webhook, generic webhook) with encrypted credentials, a fixed event
// catalog with per-event severity and per-channel routing, quiet hours
// with an undisable critical-alert override, and a durable delivery queue
// that retries and logs failures rather than dropping them.
//
// Nothing here has a real event source yet — SMART polling, array
// degradation detection, the mover, backups and UPS monitoring are other,
// mostly unbuilt subsystems. Service.Publish is the one exported entry
// point a future subsystem calls once it exists; internal/api wires the
// CRUD and routing operations api/openapi.yaml declares to Service and
// Store.
package notify
