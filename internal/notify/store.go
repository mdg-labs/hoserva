package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// ErrChannelNotFound is returned by Store methods addressing a channel by
// id that does not exist.
var ErrChannelNotFound = errors.New("notify: channel not found")

// Store persists channels, routing, quiet hours and the delivery queue in
// the central SQLite database (D4) through the sqlc-generated
// internal/store/db package. It has no business logic of its own —
// encryption, validation and delivery decisions belong to Service; Store
// only reads and writes rows (mirroring internal/job.Store's own split).
//
// It holds a concrete *sql.DB, not the generic storedb.DBTX every other
// Store in this codebase accepts, because SetRoute below replaces a whole
// set of notify_routes rows and needs a real transaction around that
// replace — the same reason internal/store/metrics.Store holds one.
type Store struct {
	db *sql.DB
	q  *storedb.Queries
}

// NewStore wraps db for notification persistence.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, q: storedb.New(db)}
}

// CreateChannel inserts ch as a new row, with secretCiphertext (nil if the
// channel has no credential yet) already encrypted by the caller (Q28) —
// Store never sees a plaintext credential.
func (s *Store) CreateChannel(ctx context.Context, ch *Channel, secretCiphertext []byte) error {
	config, err := json.Marshal(ch.Config)
	if err != nil {
		return fmt.Errorf("notify: encoding channel config: %w", err)
	}
	return s.q.CreateChannel(ctx, storedb.CreateChannelParams{
		ID:        ch.ID,
		Name:      ch.Name,
		Type:      string(ch.Type),
		Enabled:   boolToSQL(ch.Enabled),
		Config:    string(config),
		Secret:    secretCiphertext,
		CreatedAt: ch.CreatedAt.UTC().Format(store.TimeFormat),
		UpdatedAt: ch.UpdatedAt.UTC().Format(store.TimeFormat),
	})
}

// GetChannel returns the channel with the given id, or ErrChannelNotFound.
// It never returns a credential — HasSecret is the only trace of one.
func (s *Store) GetChannel(ctx context.Context, id string) (*Channel, error) {
	row, err := s.q.GetChannel(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrChannelNotFound
		}
		return nil, err
	}
	return channelFromRow(row)
}

// getChannelSecretCiphertext returns id's stored credential ciphertext
// (nil if it has none), for Service's own internal use when it needs to
// decrypt a credential to send through it or to carry an unchanged one
// forward across an update. Unexported: a caller outside this package
// reaches a channel's credential only by way of Service, never Store
// directly.
func (s *Store) getChannelSecretCiphertext(ctx context.Context, id string) ([]byte, error) {
	row, err := s.q.GetChannel(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrChannelNotFound
		}
		return nil, err
	}
	return row.Secret, nil
}

// ListChannels returns every configured channel, oldest first.
func (s *Store) ListChannels(ctx context.Context) ([]*Channel, error) {
	rows, err := s.q.ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Channel, 0, len(rows))
	for _, row := range rows {
		ch, err := channelFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}

// UpdateChannel replaces id's name/type/enabled/config and credential in
// one statement — secretCiphertext is whatever the caller has already
// decided the new stored value should be (the existing one carried
// forward, a new one, or nil to clear), never resolved by Store itself.
func (s *Store) UpdateChannel(ctx context.Context, ch *Channel, secretCiphertext []byte) error {
	config, err := json.Marshal(ch.Config)
	if err != nil {
		return fmt.Errorf("notify: encoding channel config: %w", err)
	}
	n, err := s.q.UpdateChannel(ctx, storedb.UpdateChannelParams{
		Name:      ch.Name,
		Type:      string(ch.Type),
		Enabled:   boolToSQL(ch.Enabled),
		Config:    string(config),
		Secret:    secretCiphertext,
		UpdatedAt: ch.UpdatedAt.UTC().Format(store.TimeFormat),
		ID:        ch.ID,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrChannelNotFound
	}
	return nil
}

// DeleteChannel removes id, along with every notify_routes and
// notify_deliveries row that named it. The deletes run explicitly inside
// one transaction rather than relying on schema.sql's ON DELETE CASCADE:
// SQLite only honours a foreign key action on a connection with
// "PRAGMA foreign_keys = ON", and store.DSN's pooled *sql.DB can hand any
// query to any connection, so a cascade alone is not a guarantee here.
func (s *Store) DeleteChannel(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("notify: beginning channel delete transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := s.q.WithTx(tx)
	if err := q.DeleteDeliveriesForChannel(ctx, id); err != nil {
		return fmt.Errorf("notify: deleting channel deliveries: %w", err)
	}
	if err := q.DeleteRoutesForChannel(ctx, id); err != nil {
		return fmt.Errorf("notify: deleting channel routes: %w", err)
	}
	n, err := q.DeleteChannel(ctx, id)
	if err != nil {
		return fmt.Errorf("notify: deleting channel: %w", err)
	}
	if n == 0 {
		return ErrChannelNotFound
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("notify: committing channel delete: %w", err)
	}
	return nil
}

// HasEncryptedSecrets implements auth.MachineKeyStore's own check,
// combined with the users table's by internal/api.AuthStore (Q28): whether
// any notify_channels row already holds a credential encrypted under some
// machine key.
func (s *Store) HasEncryptedSecrets(ctx context.Context) (bool, error) {
	return s.q.HasEncryptedNotifySecrets(ctx)
}

// RoutingEntry is one event type's full routing row (doc 03 §8.3's
// matrix): its effective severity (an override if one exists, otherwise
// the compiled-in default) and every channel id currently routed to it.
type RoutingEntry struct {
	EventType  EventType
	Severity   Severity
	ChannelIDs []string
}

// GetRouting returns one RoutingEntry per EventCatalog member, in that
// slice's order, so every event type appears even before it has ever been
// routed or had its severity overridden.
func (s *Store) GetRouting(ctx context.Context) ([]RoutingEntry, error) {
	overrides, err := s.q.ListEventSeverityOverrides(ctx)
	if err != nil {
		return nil, err
	}
	overrideByEvent := make(map[EventType]Severity, len(overrides))
	for _, o := range overrides {
		overrideByEvent[EventType(o.EventType)] = Severity(o.Severity)
	}

	routes, err := s.q.ListAllRoutes(ctx)
	if err != nil {
		return nil, err
	}
	channelsByEvent := make(map[EventType][]string)
	for _, r := range routes {
		et := EventType(r.EventType)
		channelsByEvent[et] = append(channelsByEvent[et], r.ChannelID)
	}

	out := make([]RoutingEntry, 0, len(EventCatalog))
	for _, event := range EventCatalog {
		severity, ok := overrideByEvent[event]
		if !ok {
			severity, _ = DefaultSeverity(event)
		}
		out = append(out, RoutingEntry{
			EventType:  event,
			Severity:   severity,
			ChannelIDs: channelsByEvent[event],
		})
	}
	return out, nil
}

// GetEffectiveSeverity returns event's current severity: an override if
// notify_event_severity has one, otherwise its compiled-in default.
func (s *Store) GetEffectiveSeverity(ctx context.Context, event EventType) (Severity, error) {
	severity, err := s.q.GetEventSeverityOverride(ctx, string(event))
	if err == nil {
		return Severity(severity), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	def, ok := DefaultSeverity(event)
	if !ok {
		return "", fmt.Errorf("notify: unknown event type %q has no compiled-in default severity", event)
	}
	return def, nil
}

// SetRoute replaces event's own routing row: its severity becomes exactly
// severity (recorded as an override even when it matches the compiled-in
// default — simpler and no less correct than special-casing that match),
// and its channel list becomes exactly channelIDs. Atomic: a transaction
// so a reader never observes the old channel list paired with the new
// severity, or a partially replaced channel list.
func (s *Store) SetRoute(ctx context.Context, event EventType, severity Severity, channelIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("notify: beginning route transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := s.q.WithTx(tx)
	if err := q.UpsertEventSeverity(ctx, storedb.UpsertEventSeverityParams{
		EventType: string(event),
		Severity:  string(severity),
	}); err != nil {
		return fmt.Errorf("notify: setting event severity: %w", err)
	}
	if err := q.DeleteRoutesForEvent(ctx, string(event)); err != nil {
		return fmt.Errorf("notify: clearing existing routes: %w", err)
	}
	for _, channelID := range channelIDs {
		if err := q.InsertRoute(ctx, storedb.InsertRouteParams{
			EventType: string(event),
			ChannelID: channelID,
		}); err != nil {
			return fmt.Errorf("notify: routing to channel %s: %w", channelID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("notify: committing route change: %w", err)
	}
	return nil
}

// ChannelsForEvent returns every enabled channel currently routed to
// event, oldest first — what Service.Publish delivers to.
func (s *Store) ChannelsForEvent(ctx context.Context, event EventType) ([]*Channel, error) {
	rows, err := s.q.ListChannelsForEvent(ctx, string(event))
	if err != nil {
		return nil, err
	}
	out := make([]*Channel, 0, len(rows))
	for _, row := range rows {
		ch, err := channelFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}

// QuietHours is notify_quiet_hours' one row (doc 03 §8.3). The
// "critical alerts always deliver" override is not a field here — it is
// not configuration at all, it is Service's own hardcoded delivery rule.
type QuietHours struct {
	Enabled   bool
	StartTime string // "HH:MM", 24-hour, local time
	EndTime   string
	UpdatedAt time.Time
}

// defaultQuietHours is what GetQuietHours returns before any
// updateQuietHours call has ever happened — disabled, so a fresh install
// delivers everything immediately rather than silently suppressing
// notifications for a window nobody configured.
var defaultQuietHours = QuietHours{Enabled: false, StartTime: "22:00", EndTime: "07:00"}

// GetQuietHours returns the current configuration, or defaultQuietHours
// if it has never been set.
func (s *Store) GetQuietHours(ctx context.Context) (QuietHours, error) {
	row, err := s.q.GetQuietHours(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return defaultQuietHours, nil
		}
		return QuietHours{}, err
	}
	updatedAt, err := time.Parse(store.TimeFormat, row.UpdatedAt)
	if err != nil {
		return QuietHours{}, fmt.Errorf("notify: parsing quiet hours updated_at: %w", err)
	}
	return QuietHours{
		Enabled:   row.Enabled != 0,
		StartTime: row.StartTime,
		EndTime:   row.EndTime,
		UpdatedAt: updatedAt,
	}, nil
}

// SetQuietHours upserts the single quiet-hours row.
func (s *Store) SetQuietHours(ctx context.Context, qh QuietHours) error {
	return s.q.SetQuietHours(ctx, storedb.SetQuietHoursParams{
		Enabled:   boolToSQL(qh.Enabled),
		StartTime: qh.StartTime,
		EndTime:   qh.EndTime,
		UpdatedAt: qh.UpdatedAt.UTC().Format(store.TimeFormat),
	})
}

// Delivery is a notify_deliveries row: one notification owed to one
// channel.
type Delivery struct {
	ID            string
	ChannelID     string
	EventType     EventType
	Severity      Severity
	Title         string
	Message       string
	Status        DeliveryStatus
	Attempts      int
	LastError     string
	CreatedAt     time.Time
	NextAttemptAt time.Time
	DeliveredAt   *time.Time
}

// DeliveryStatus is a notify_deliveries row's own lifecycle state.
type DeliveryStatus string

const (
	// DeliveryPending is owed and either not yet attempted or awaiting its
	// next retry at NextAttemptAt.
	DeliveryPending DeliveryStatus = "pending"
	// DeliveryDelivered succeeded. Terminal.
	DeliveryDelivered DeliveryStatus = "delivered"
	// DeliveryFailed exhausted its retries. Terminal, but never silently
	// dropped — the row, its attempt count and its last error stay on
	// record.
	DeliveryFailed DeliveryStatus = "failed"
	// DeliverySuppressed was never attempted at all because quiet hours
	// were active and its severity was not critical. Terminal, and
	// recorded for the same reason DeliveryFailed is: "why nothing was
	// sent" is never silent either.
	DeliverySuppressed DeliveryStatus = "suppressed"
)

// CreateDelivery inserts d as a new row. d.ID, d.CreatedAt and
// d.NextAttemptAt must already be set.
func (s *Store) CreateDelivery(ctx context.Context, d *Delivery) error {
	return s.q.CreateDelivery(ctx, storedb.CreateDeliveryParams{
		ID:            d.ID,
		ChannelID:     d.ChannelID,
		EventType:     string(d.EventType),
		Severity:      string(d.Severity),
		Title:         d.Title,
		Message:       d.Message,
		Status:        string(d.Status),
		Attempts:      int64(d.Attempts),
		LastError:     stringToSQL(d.LastError),
		CreatedAt:     d.CreatedAt.UTC().Format(store.TimeFormat),
		NextAttemptAt: d.NextAttemptAt.UTC().Format(store.TimeFormat),
		DeliveredAt:   timeToSQL(d.DeliveredAt),
	})
}

// ListDueDeliveries returns up to limit pending deliveries whose
// NextAttemptAt is at or before now, oldest first — what Worker.RunOnce
// attempts next.
func (s *Store) ListDueDeliveries(ctx context.Context, now time.Time, limit int) ([]*Delivery, error) {
	rows, err := s.q.ListDueDeliveries(ctx, storedb.ListDueDeliveriesParams{
		NextAttemptAt: now.UTC().Format(store.TimeFormat),
		RowLimit:      int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]*Delivery, 0, len(rows))
	for _, row := range rows {
		d, err := deliveryFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// UpdateDeliveryAttempt persists a delivery's outcome after one attempt —
// its new status, attempt count, last error (cleared on success) and, for
// a retry, when to try again.
func (s *Store) UpdateDeliveryAttempt(ctx context.Context, d *Delivery) error {
	return s.q.UpdateDeliveryAttempt(ctx, storedb.UpdateDeliveryAttemptParams{
		Status:        string(d.Status),
		Attempts:      int64(d.Attempts),
		LastError:     stringToSQL(d.LastError),
		NextAttemptAt: d.NextAttemptAt.UTC().Format(store.TimeFormat),
		DeliveredAt:   timeToSQL(d.DeliveredAt),
		ID:            d.ID,
	})
}

func channelFromRow(row *storedb.NotifyChannel) (*Channel, error) {
	var config ChannelConfig
	if err := json.Unmarshal([]byte(row.Config), &config); err != nil {
		return nil, fmt.Errorf("notify: decoding config for channel %s: %w", row.ID, err)
	}
	createdAt, err := time.Parse(store.TimeFormat, row.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("notify: parsing created_at for channel %s: %w", row.ID, err)
	}
	updatedAt, err := time.Parse(store.TimeFormat, row.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("notify: parsing updated_at for channel %s: %w", row.ID, err)
	}
	return &Channel{
		ID:        row.ID,
		Name:      row.Name,
		Type:      ChannelType(row.Type),
		Enabled:   row.Enabled != 0,
		Config:    config,
		HasSecret: row.Secret != nil,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}, nil
}

func deliveryFromRow(row *storedb.NotifyDelivery) (*Delivery, error) {
	createdAt, err := time.Parse(store.TimeFormat, row.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("notify: parsing created_at for delivery %s: %w", row.ID, err)
	}
	nextAttemptAt, err := time.Parse(store.TimeFormat, row.NextAttemptAt)
	if err != nil {
		return nil, fmt.Errorf("notify: parsing next_attempt_at for delivery %s: %w", row.ID, err)
	}
	deliveredAt, err := sqlToTime(row.DeliveredAt)
	if err != nil {
		return nil, fmt.Errorf("notify: parsing delivered_at for delivery %s: %w", row.ID, err)
	}
	return &Delivery{
		ID:            row.ID,
		ChannelID:     row.ChannelID,
		EventType:     EventType(row.EventType),
		Severity:      Severity(row.Severity),
		Title:         row.Title,
		Message:       row.Message,
		Status:        DeliveryStatus(row.Status),
		Attempts:      int(row.Attempts),
		LastError:     row.LastError.String,
		CreatedAt:     createdAt,
		NextAttemptAt: nextAttemptAt,
		DeliveredAt:   deliveredAt,
	}, nil
}

func boolToSQL(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func stringToSQL(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func timeToSQL(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: t.UTC().Format(store.TimeFormat), Valid: true}
}

func sqlToTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	t, err := time.Parse(store.TimeFormat, v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
