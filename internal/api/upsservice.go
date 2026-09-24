package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/user"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
)

// ErrInvalidUPSInput is returned when a UPS settings update fails
// validation before any file or database write.
var ErrInvalidUPSInput = errors.New("settings: invalid ups input")

// ErrMissingNUTGroup is returned when USB mode needs the Debian nut
// group and it is not present on the host — refused before any NUT
// file is written.
var ErrMissingNUTGroup = errors.New("settings: the nut group is not present on this host — install the nut package before configuring a USB UPS")

// NUTReloader reloads the NUT systemd units that match a connection mode
// after WriteUPS lands new files. cmd/hoservad implements this; tests
// inject a fake.
type NUTReloader interface {
	Reload(ctx context.Context, connection config.UPSConnection) error
}

// UPSSocketPermissions re-applies the ups control socket's nut-group
// ownership (#340) on every UPS settings save. cmd/hoservad's own
// daemon-startup chown is one-shot: nut is only Recommends:, not
// Depends:, so it can be installed after hoservad already started and
// found no group to chow to. This is the only later point the daemon
// re-checks, covering that case without a timer walking anything
// (CLAUDE.md). cmd/hoservad implements this; tests inject a fake.
type UPSSocketPermissions interface {
	Apply(ctx context.Context) error
}

// UPSService is UPS settings business logic (#249): persist to SQLite,
// generate NUT config through Generator.WriteUPS (D4, Q77), reload NUT
// units, and re-apply the ups control socket's nut-group ownership.
// Passwords are encrypted at rest (Q28).
type UPSService struct {
	Store     *UPSStore
	Cipher    SettingsCipher
	Generator *config.Generator
	NUT       NUTReloader
	Socket    UPSSocketPermissions
	Now       func() time.Time
}

// NewUPSService wires a UPSService with the real clock.
func NewUPSService(store *UPSStore, cipher SettingsCipher, generator *config.Generator, nut NUTReloader, socket UPSSocketPermissions) *UPSService {
	return &UPSService{
		Store:     store,
		Cipher:    cipher,
		Generator: generator,
		NUT:       nut,
		Socket:    socket,
		Now:       time.Now,
	}
}

func (s *UPSService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// UPSView is the API-facing UPS settings (passwords never included).
type UPSView struct {
	Configured         bool
	Connection         config.UPSConnection
	Driver             string
	Port               string
	MonitorPasswordSet bool
	NetworkHost        string
	NetworkPort        int
	NetworkUPSName     string
	NetworkUsername    string
	NetworkPasswordSet bool
	LowBatteryPercent  int
	RuntimeSeconds     int
}

// UpdateUPSInput is a PUT /settings/ups body. Nil password pointers keep
// any already-stored secret; a first configure must supply the password
// the connection mode needs.
type UpdateUPSInput struct {
	Connection        config.UPSConnection
	Driver            string
	Port              string
	MonitorPassword   *string
	NetworkHost       string
	NetworkPort       int
	NetworkUPSName    string
	NetworkUsername   string
	NetworkPassword   *string
	LowBatteryPercent int
	RuntimeSeconds    int
}

// Get returns the current UPS settings, or Configured=false when none.
func (s *UPSService) Get(ctx context.Context) (UPSView, error) {
	row, err := s.Store.Get(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UPSView{}, nil
		}
		return UPSView{}, fmt.Errorf("settings: loading ups config: %w", err)
	}
	return upsViewFromRow(row), nil
}

// Update validates, persists, generates NUT config, and reloads units.
// On WriteUPS or reload failure SQLite and the generated NUT files are
// both rolled back so GET never shows configured:false while live NUT
// still runs Hoserva-managed config (and vice versa).
func (s *UPSService) Update(ctx context.Context, input UpdateUPSInput) (UPSView, error) {
	if s.Generator == nil {
		return UPSView{}, fmt.Errorf("settings: ups generator is not configured")
	}
	if s.Cipher == nil {
		return UPSView{}, fmt.Errorf("settings: a SettingsCipher is required to store ups passwords")
	}

	previous, err := s.Store.Get(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return UPSView{}, fmt.Errorf("settings: loading ups config: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		previous = nil
	}

	state, row, err := s.buildState(input, previous)
	if err != nil {
		return UPSView{}, err
	}

	if err := s.Generator.CanWriteUPS(ctx, state); err != nil {
		return UPSView{}, mapUPSGeneratorError(err)
	}

	// One instant for the row and the generated header. Drift detection
	// hashes the whole file, including "Generated:", so a later rollback
	// must rewrite those files with this same timestamp.
	now := s.now()
	row.UpdatedAt = now.UTC().Format(timeFormat)
	if err := s.Store.Upsert(ctx, row); err != nil {
		return UPSView{}, fmt.Errorf("settings: saving ups config: %w", err)
	}

	if err := s.Generator.WriteUPS(ctx, state, "settings ups", 1, now); err != nil {
		if rbErr := s.rollbackUPS(previous); rbErr != nil {
			return UPSView{}, fmt.Errorf("settings: writing nut config: %w (rollback also failed: %v)", mapUPSGeneratorError(err), rbErr)
		}
		return UPSView{}, mapUPSGeneratorError(err)
	}

	// Before NUT.Reload below restarts nut-monitor: upsmon's own nut
	// child must find the ups control socket already reachable the
	// moment it (re)starts, not moments later.
	if s.Socket != nil {
		if err := s.Socket.Apply(ctx); err != nil {
			if rbErr := s.rollbackUPS(previous); rbErr != nil {
				return UPSView{}, fmt.Errorf("settings: applying ups control socket permissions: %w (rollback also failed: %v)", err, rbErr)
			}
			return UPSView{}, fmt.Errorf("settings: applying ups control socket permissions: %w", err)
		}
	}

	if s.NUT != nil {
		if err := s.NUT.Reload(ctx, state.Connection); err != nil {
			if rbErr := s.rollbackUPS(previous); rbErr != nil {
				return UPSView{}, fmt.Errorf("settings: reloading nut: %w (rollback also failed: %v)", err, rbErr)
			}
			return UPSView{}, fmt.Errorf("settings: reloading nut: %w", err)
		}
	}

	return upsViewFromRow(&row), nil
}

// rollbackUPS restores SQLite and the generated NUT files to previous
// (or clears both when previous is nil). It uses its own context so a
// cancelled request cannot leave a half-applied change behind.
func (s *UPSService) rollbackUPS(previous *UPSConfigRow) error {
	ctx := context.Background()
	filesErr := s.restoreUPSGenerated(ctx, previous)
	dbErr := restoreUPSRow(s.Store, previous)
	switch {
	case filesErr != nil && dbErr != nil:
		return fmt.Errorf("files: %v; database: %v", filesErr, dbErr)
	case filesErr != nil:
		return filesErr
	case dbErr != nil:
		return dbErr
	default:
		return nil
	}
}

// restoreUPSGenerated rewrites prior NUT files from previous, or
// RemoveManageds every UPS path when previous is nil (first configure).
func (s *UPSService) restoreUPSGenerated(ctx context.Context, previous *UPSConfigRow) error {
	if previous == nil {
		for _, path := range []string{
			config.PathNUTConf,
			config.PathUPSMonConf,
			config.PathUPSConf,
			config.PathUPSDUsers,
		} {
			if _, err := s.Generator.RemoveManaged(ctx, path); err != nil {
				return fmt.Errorf("settings: removing generated %s: %w", path, err)
			}
		}
		return nil
	}
	state, err := s.stateFromRow(previous)
	if err != nil {
		return err
	}
	generatedAt, err := time.Parse(timeFormat, previous.UpdatedAt)
	if err != nil {
		return fmt.Errorf("settings: restoring nut config: %w", err)
	}
	if err := s.Generator.WriteUPS(ctx, state, "settings ups", 1, generatedAt); err != nil {
		return fmt.Errorf("settings: restoring nut config: %w", mapUPSGeneratorError(err))
	}
	return nil
}

func (s *UPSService) stateFromRow(row *UPSConfigRow) (config.UPSState, error) {
	state := config.UPSState{Connection: config.UPSConnection(row.Connection)}
	switch state.Connection {
	case config.UPSConnectionUSB:
		plain, err := s.Cipher.Decrypt(row.MonitorPassword)
		if err != nil {
			return config.UPSState{}, fmt.Errorf("settings: decrypting stored monitor password: %w", err)
		}
		state.Driver = row.Driver
		state.Port = row.Port
		state.MonitorPassword = string(plain)
		state.LowBatteryPercent = int(row.LowBatteryPercent)
		state.RuntimeSeconds = int(row.RuntimeSeconds)
	case config.UPSConnectionNetwork:
		plain, err := s.Cipher.Decrypt(row.NetworkPassword)
		if err != nil {
			return config.UPSState{}, fmt.Errorf("settings: decrypting stored network password: %w", err)
		}
		state.NetworkHost = row.NetworkHost
		state.NetworkPort = int(row.NetworkPort)
		state.NetworkUPSName = row.NetworkUPSName
		state.NetworkUsername = row.NetworkUsername
		state.NetworkPassword = string(plain)
	default:
		return config.UPSState{}, fmt.Errorf("settings: unknown ups connection %q in stored row", row.Connection)
	}
	return state, nil
}

func (s *UPSService) buildState(input UpdateUPSInput, previous *UPSConfigRow) (config.UPSState, UPSConfigRow, error) {
	switch input.Connection {
	case config.UPSConnectionUSB, config.UPSConnectionNetwork:
	default:
		return config.UPSState{}, UPSConfigRow{}, fmt.Errorf("%w: connection must be usb or network", ErrInvalidUPSInput)
	}

	row := UPSConfigRow{Connection: string(input.Connection)}
	state := config.UPSState{Connection: input.Connection}

	switch input.Connection {
	case config.UPSConnectionUSB:
		driver := strings.TrimSpace(input.Driver)
		port := strings.TrimSpace(input.Port)
		if driver == "" {
			return config.UPSState{}, UPSConfigRow{}, fmt.Errorf("%w: a USB UPS needs a NUT driver name (for example usbhid-ups)", ErrInvalidUPSInput)
		}
		if port == "" {
			return config.UPSState{}, UPSConfigRow{}, fmt.Errorf("%w: a USB UPS needs a driver port (commonly auto)", ErrInvalidUPSInput)
		}
		if input.LowBatteryPercent < 0 || input.LowBatteryPercent > 100 {
			return config.UPSState{}, UPSConfigRow{}, fmt.Errorf("%w: low battery percent must be between 0 and 100", ErrInvalidUPSInput)
		}
		if input.RuntimeSeconds < 0 {
			return config.UPSState{}, UPSConfigRow{}, fmt.Errorf("%w: runtime on battery must not be negative", ErrInvalidUPSInput)
		}

		monitorPlain, err := s.resolvePassword(input.MonitorPassword, previous, true)
		if err != nil {
			return config.UPSState{}, UPSConfigRow{}, err
		}
		monitorCipher, err := s.Cipher.Encrypt([]byte(monitorPlain))
		if err != nil {
			return config.UPSState{}, UPSConfigRow{}, fmt.Errorf("settings: encrypting monitor password: %w", err)
		}

		row.Driver = driver
		row.Port = port
		row.MonitorPassword = monitorCipher
		row.NetworkPassword = []byte{}
		row.LowBatteryPercent = int64(input.LowBatteryPercent)
		row.RuntimeSeconds = int64(input.RuntimeSeconds)

		state.Driver = driver
		state.Port = port
		state.MonitorPassword = monitorPlain
		state.LowBatteryPercent = input.LowBatteryPercent
		state.RuntimeSeconds = input.RuntimeSeconds

	case config.UPSConnectionNetwork:
		host := strings.TrimSpace(input.NetworkHost)
		upsName := strings.TrimSpace(input.NetworkUPSName)
		username := strings.TrimSpace(input.NetworkUsername)
		if host == "" {
			return config.UPSState{}, UPSConfigRow{}, fmt.Errorf("%w: a network UPS needs the NUT server hostname", ErrInvalidUPSInput)
		}
		if upsName == "" {
			return config.UPSState{}, UPSConfigRow{}, fmt.Errorf("%w: a network UPS needs the UPS name on that NUT server", ErrInvalidUPSInput)
		}
		if username == "" {
			return config.UPSState{}, UPSConfigRow{}, fmt.Errorf("%w: a network UPS needs the monitoring username", ErrInvalidUPSInput)
		}
		port := input.NetworkPort
		if port == 0 {
			port = 3493
		}
		if port < 1 || port > 65535 {
			return config.UPSState{}, UPSConfigRow{}, fmt.Errorf("%w: network port must be between 1 and 65535", ErrInvalidUPSInput)
		}

		networkPlain, err := s.resolvePassword(input.NetworkPassword, previous, false)
		if err != nil {
			return config.UPSState{}, UPSConfigRow{}, err
		}
		networkCipher, err := s.Cipher.Encrypt([]byte(networkPlain))
		if err != nil {
			return config.UPSState{}, UPSConfigRow{}, fmt.Errorf("settings: encrypting network password: %w", err)
		}

		row.NetworkHost = host
		row.NetworkPort = int64(port)
		row.NetworkUPSName = upsName
		row.NetworkUsername = username
		row.NetworkPassword = networkCipher
		row.MonitorPassword = []byte{}

		state.NetworkHost = host
		state.NetworkPort = port
		state.NetworkUPSName = upsName
		state.NetworkUsername = username
		state.NetworkPassword = networkPlain
	}

	return state, row, nil
}

func (s *UPSService) resolvePassword(supplied *string, previous *UPSConfigRow, usb bool) (string, error) {
	needUSB := "%w: a USB UPS needs a monitor password"
	needNet := "%w: a network UPS needs the monitoring password"
	need := needNet
	if usb {
		need = needUSB
	}
	if supplied != nil {
		if *supplied == "" {
			return "", fmt.Errorf(need, ErrInvalidUPSInput)
		}
		return *supplied, nil
	}
	want := config.UPSConnectionNetwork
	cipher := []byte(nil)
	label := "network"
	if usb {
		want = config.UPSConnectionUSB
		label = "monitor"
	}
	if previous != nil && previous.Connection == string(want) {
		if usb {
			cipher = previous.MonitorPassword
		} else {
			cipher = previous.NetworkPassword
		}
	}
	if len(cipher) == 0 {
		return "", fmt.Errorf(need, ErrInvalidUPSInput)
	}
	plain, err := s.Cipher.Decrypt(cipher)
	if err != nil {
		return "", fmt.Errorf("settings: decrypting stored %s password: %w", label, err)
	}
	return string(plain), nil
}

func upsViewFromRow(row *UPSConfigRow) UPSView {
	view := UPSView{
		Configured:         true,
		Connection:         config.UPSConnection(row.Connection),
		Driver:             row.Driver,
		Port:               row.Port,
		MonitorPasswordSet: len(row.MonitorPassword) > 0,
		NetworkHost:        row.NetworkHost,
		NetworkPort:        int(row.NetworkPort),
		NetworkUPSName:     row.NetworkUPSName,
		NetworkUsername:    row.NetworkUsername,
		NetworkPasswordSet: len(row.NetworkPassword) > 0,
		LowBatteryPercent:  int(row.LowBatteryPercent),
		RuntimeSeconds:     int(row.RuntimeSeconds),
	}
	return view
}

func mapUPSGeneratorError(err error) error {
	var unknown user.UnknownGroupError
	if errors.As(err, &unknown) && string(unknown) == config.NUTGroup {
		return ErrMissingNUTGroup
	}
	if strings.Contains(err.Error(), `resolving group "`+config.NUTGroup+`"`) {
		return ErrMissingNUTGroup
	}
	if errors.Is(err, config.ErrInvalidUPSField) {
		return fmt.Errorf("%w: %v", ErrInvalidUPSInput, err)
	}
	return err
}
