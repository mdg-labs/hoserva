package notify

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Service is internal/notify's own exported entry point (#35): channel
// CRUD with encrypted credentials (Q28), the per-event routing matrix,
// quiet hours, "send test notification", and Publish — the
// publish-an-event-from-anywhere API a future subsystem (SMART polling,
// array degradation detection, the mover, backups, UPS monitoring) calls
// once it exists. internal/api's handlers call only this type, never
// Store directly, mirroring AuthService's own split from AuthStore.
type Service struct {
	Store   *Store
	Cipher  SecretCipher
	Senders Senders

	// Now defaults to time.Now. Tests inject a fixed clock so quiet-hours
	// suppression is deterministic.
	Now func() time.Time
	// NewID defaults to uuid.NewString.
	NewID func() string
	// MaxAttempts is how many times Worker retries a failing delivery
	// before marking it permanently failed (still logged, never dropped —
	// see DeliveryFailed). Defaults to DefaultMaxAttempts.
	MaxAttempts int
	// Log receives one line per delivery attempt outcome — defaults to
	// log.Printf. Never the only record of an attempt: every one is also
	// persisted (UpdateDeliveryAttempt) before this is called.
	Log func(format string, args ...any)
}

// DefaultMaxAttempts is Service.MaxAttempts' default: five attempts
// before a delivery is marked permanently failed.
const DefaultMaxAttempts = 5

// NewService wires a Service with the real clock, uuid.NewString and
// DefaultMaxAttempts.
func NewService(store *Store, cipher SecretCipher, senders Senders) *Service {
	return &Service{
		Store:       store,
		Cipher:      cipher,
		Senders:     senders,
		Now:         time.Now,
		NewID:       uuid.NewString,
		MaxAttempts: DefaultMaxAttempts,
		Log:         defaultLog,
	}
}

func defaultLog(format string, args ...any) {
	fmt.Printf("notify: "+format+"\n", args...)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}
	return uuid.NewString()
}

func (s *Service) log(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

// ListChannels returns every configured channel.
func (s *Service) ListChannels(ctx context.Context) ([]*Channel, error) {
	return s.Store.ListChannels(ctx)
}

// GetChannel returns one channel by id.
func (s *Service) GetChannel(ctx context.Context, id string) (*Channel, error) {
	return s.Store.GetChannel(ctx, id)
}

// ErrInvalidInput wraps every validation failure Service itself detects
// (an invalid ChannelInput, an unknown event type, an invalid severity or
// quiet-hours time) — internal/api classifies it as a 400, distinct from
// ErrChannelNotFound/ErrUnknownChannel (which name a specific id) and
// from an unwrapped error (an infrastructure failure, a 500).
var ErrInvalidInput = errors.New("notify: invalid input")

// CreateChannel validates in, encrypts its credential if one is supplied
// (Q28), and persists a new channel.
func (s *Service) CreateChannel(ctx context.Context, in ChannelInput) (*Channel, error) {
	if err := in.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	ciphertext, err := s.resolveSecretForCreate(in.Secret)
	if err != nil {
		return nil, err
	}

	now := s.now()
	ch := &Channel{
		ID:        s.newID(),
		Name:      in.Name,
		Type:      in.Type,
		Enabled:   in.Enabled,
		Config:    in.Config,
		HasSecret: ciphertext != nil,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.Store.CreateChannel(ctx, ch, ciphertext); err != nil {
		return nil, fmt.Errorf("notify: creating channel: %w", err)
	}
	return ch, nil
}

// resolveSecretForCreate encrypts a newly supplied credential. Create has
// nothing to "keep" yet, so SecretKeep and a nil input both mean no
// credential at all — only SecretSet produces ciphertext.
func (s *Service) resolveSecretForCreate(in *SecretInput) ([]byte, error) {
	if in == nil || in.Op != SecretSet {
		return nil, nil
	}
	return encryptSecret(s.Cipher, in.Value)
}

// UpdateChannel validates in, resolves what its credential should become
// (kept, cleared or replaced — Q28), and replaces id's row entirely.
func (s *Service) UpdateChannel(ctx context.Context, id string, in ChannelInput) (*Channel, error) {
	if err := in.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	ciphertext, err := s.resolveSecretForUpdate(ctx, id, in.Secret)
	if err != nil {
		return nil, err
	}

	ch := &Channel{
		ID:        id,
		Name:      in.Name,
		Type:      in.Type,
		Enabled:   in.Enabled,
		Config:    in.Config,
		UpdatedAt: s.now(),
	}
	if err := s.Store.UpdateChannel(ctx, ch, ciphertext); err != nil {
		return nil, fmt.Errorf("notify: updating channel %s: %w", id, err)
	}
	return s.Store.GetChannel(ctx, id)
}

// resolveSecretForUpdate decides the ciphertext UpdateChannel writes: the
// existing one carried forward (SecretKeep, or no Secret supplied at
// all), nil (SecretClear), or a freshly encrypted one (SecretSet).
func (s *Service) resolveSecretForUpdate(ctx context.Context, id string, in *SecretInput) ([]byte, error) {
	if in == nil || in.Op == SecretKeep {
		return s.Store.getChannelSecretCiphertext(ctx, id)
	}
	if in.Op == SecretClear {
		return nil, nil
	}
	return encryptSecret(s.Cipher, in.Value)
}

// DeleteChannel removes a channel and every routing entry that named it.
func (s *Service) DeleteChannel(ctx context.Context, id string) error {
	return s.Store.DeleteChannel(ctx, id)
}

// TestResult is what sendTestNotification (doc 03 §8.3) reports:
// "untested notification config is the same as no notification config",
// so this always succeeds at the HTTP layer — Success/Error is the
// signal, not a thrown error, for anything the channel's own server
// itself rejected.
type TestResult struct {
	Success bool
	Error   string
}

// TestChannel sends a synthetic notification through id's channel
// immediately, outside the delivery queue and its retry policy — a
// direct probe of the channel's own configuration and credential. An
// error return is an infrastructure failure (channel not found, its
// credential can't be decrypted); the channel's own delivery failure
// comes back as a non-nil *TestResult with Success false instead.
func (s *Service) TestChannel(ctx context.Context, id string) (*TestResult, error) {
	ch, err := s.Store.GetChannel(ctx, id)
	if err != nil {
		return nil, err
	}
	secret, err := s.decryptChannelSecret(ctx, id)
	if err != nil {
		return nil, err
	}
	sender, ok := s.Senders[ch.Type]
	if !ok {
		return nil, fmt.Errorf("notify: no sender registered for channel type %q", ch.Type)
	}

	msg := Message{
		EventType: EventTest,
		Severity:  SeverityInfo,
		Title:     "Hoserva test notification",
		Body:      fmt.Sprintf("This is a test notification from Hoserva for the %q channel.", ch.Name),
	}
	if err := sender.Send(ctx, ch.Config, secret, msg); err != nil {
		return &TestResult{Success: false, Error: err.Error()}, nil
	}
	return &TestResult{Success: true}, nil
}

// decryptChannelSecret returns id's decrypted credential, or "" if it has
// none.
func (s *Service) decryptChannelSecret(ctx context.Context, id string) (string, error) {
	ciphertext, err := s.Store.getChannelSecretCiphertext(ctx, id)
	if err != nil {
		return "", err
	}
	if ciphertext == nil {
		return "", nil
	}
	return decryptSecret(s.Cipher, ciphertext)
}

// GetRouting returns doc 03 §8.3's full routing matrix.
func (s *Service) GetRouting(ctx context.Context) ([]RoutingEntry, error) {
	return s.Store.GetRouting(ctx)
}

// ErrUnknownChannel is returned by SetRoute when channelIds names a
// channel that does not exist.
var ErrUnknownChannel = errors.New("notify: unknown channel id")

// SetRoute replaces event's severity and routed-channel list in one call
// (doc 03 §8.3's per-row edit).
func (s *Service) SetRoute(ctx context.Context, event EventType, severity Severity, channelIDs []string) (*RoutingEntry, error) {
	if !ValidEventType(event) {
		return nil, fmt.Errorf("%w: unknown event type %q", ErrInvalidInput, event)
	}
	if !ValidSeverity(severity) {
		return nil, fmt.Errorf("%w: invalid severity %q", ErrInvalidInput, severity)
	}
	if err := s.confirmChannelsExist(ctx, channelIDs); err != nil {
		return nil, err
	}

	if err := s.Store.SetRoute(ctx, event, severity, channelIDs); err != nil {
		return nil, err
	}
	return &RoutingEntry{EventType: event, Severity: severity, ChannelIDs: channelIDs}, nil
}

func (s *Service) confirmChannelsExist(ctx context.Context, channelIDs []string) error {
	if len(channelIDs) == 0 {
		return nil
	}
	channels, err := s.Store.ListChannels(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(channels))
	for _, ch := range channels {
		known[ch.ID] = true
	}
	for _, id := range channelIDs {
		if !known[id] {
			return fmt.Errorf("%w: %s", ErrUnknownChannel, id)
		}
	}
	return nil
}

// GetQuietHours returns the current quiet hours configuration, plus the
// undisable critical-alert override, which is never a stored value (doc
// 03 §8.3, CLAUDE.md).
func (s *Service) GetQuietHours(ctx context.Context) (QuietHours, error) {
	return s.Store.GetQuietHours(ctx)
}

// SetQuietHours validates and persists a new quiet hours window. There is
// no parameter for the critical-alert override — it cannot be set to
// anything but always-on.
func (s *Service) SetQuietHours(ctx context.Context, enabled bool, start, end string) (QuietHours, error) {
	if _, err := parseHHMM(start); err != nil {
		return QuietHours{}, fmt.Errorf("%w: invalid start time %q: %v", ErrInvalidInput, start, err)
	}
	if _, err := parseHHMM(end); err != nil {
		return QuietHours{}, fmt.Errorf("%w: invalid end time %q: %v", ErrInvalidInput, end, err)
	}
	qh := QuietHours{Enabled: enabled, StartTime: start, EndTime: end, UpdatedAt: s.now()}
	if err := s.Store.SetQuietHours(ctx, qh); err != nil {
		return QuietHours{}, err
	}
	return qh, nil
}

// Publish is the exported publish-an-event-from-anywhere API (#35): a
// future subsystem calls this with what happened, and Service resolves
// the event's effective severity, looks up every enabled channel
// currently routed to it, and queues one notify_deliveries row per
// channel — pending, or suppressed if quiet hours are active and the
// severity is not critical (which always delivers, doc 03 §8.3, and
// cannot be turned off). It never sends anything itself; Worker does
// that, so a slow or unreachable channel never blocks the caller that
// published the event.
func (s *Service) Publish(ctx context.Context, event EventType, title, message string) error {
	if !ValidEventType(event) && event != EventTest {
		return fmt.Errorf("notify: unknown event type %q", event)
	}

	severity, err := s.Store.GetEffectiveSeverity(ctx, event)
	if err != nil {
		return fmt.Errorf("notify: resolving severity for %s: %w", event, err)
	}
	channels, err := s.Store.ChannelsForEvent(ctx, event)
	if err != nil {
		return fmt.Errorf("notify: listing channels for %s: %w", event, err)
	}
	if len(channels) == 0 {
		return nil
	}
	quiet, err := s.Store.GetQuietHours(ctx)
	if err != nil {
		return fmt.Errorf("notify: reading quiet hours: %w", err)
	}

	now := s.now()
	suppress := severity != SeverityCritical && quiet.Enabled && withinQuietHours(quiet.StartTime, quiet.EndTime, now)

	var errs []error
	for _, ch := range channels {
		status := DeliveryPending
		if suppress {
			status = DeliverySuppressed
		}
		d := &Delivery{
			ID:            s.newID(),
			ChannelID:     ch.ID,
			EventType:     event,
			Severity:      severity,
			Title:         title,
			Message:       message,
			Status:        status,
			CreatedAt:     now,
			NextAttemptAt: now,
		}
		if err := s.Store.CreateDelivery(ctx, d); err != nil {
			errs = append(errs, fmt.Errorf("queuing delivery to channel %s: %w", ch.ID, err))
		}
	}
	return errors.Join(errs...)
}

// withinQuietHours reports whether now's local wall clock time falls
// inside [start, end) — HH:MM, 24-hour. A window where start > end wraps
// past midnight (e.g. 22:00 to 07:00); start == end is treated as never
// active rather than "all day", since a configuration mistake should
// suppress nothing rather than everything.
func withinQuietHours(start, end string, now time.Time) bool {
	startMin, err := parseHHMM(start)
	if err != nil {
		return false
	}
	endMin, err := parseHHMM(end)
	if err != nil {
		return false
	}
	if startMin == endMin {
		return false
	}
	nowMin := now.Hour()*60 + now.Minute()
	if startMin < endMin {
		return nowMin >= startMin && nowMin < endMin
	}
	return nowMin >= startMin || nowMin < endMin
}

// parseHHMM parses a 24-hour "HH:MM" string into minutes since midnight.
func parseHHMM(s string) (int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("expected HH:MM")
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, fmt.Errorf("expected HH between 00 and 23")
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("expected MM between 00 and 59")
	}
	return hour*60 + minute, nil
}
