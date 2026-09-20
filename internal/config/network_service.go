package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	// ErrNetworkInvalid is a caller-supplied addressing field the API rejects.
	ErrNetworkInvalid = errors.New("config: invalid network input")
	// ErrNetworkReadOnly is returned when the backend is not ifupdown (Q75).
	ErrNetworkReadOnly = errors.New("config: network backend does not allow edits")
	// ErrNetworkPending is returned when a confirm-or-revert is already running.
	ErrNetworkPending = errors.New("config: a network change is already waiting for confirm")
	// ErrNetworkConfirmExpired is returned when confirm is called with no live window.
	ErrNetworkConfirmExpired = errors.New("config: no pending network change to confirm")
)

type pendingFile struct {
	Interface       string    `json:"interface"`
	PreviousBody    []byte    `json:"previous_body"`
	PreviousExisted bool      `json:"previous_existed"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// NetworkService is the ifupdown confirm-or-revert path (Q75). Detector,
// Runner and Links are package interfaces with scriptable fakes so unit
// tests never touch a live NIC. Pending state is a JSON file under
// StateDir so a daemon crash before confirm still restores the previous
// file on the next start.
type NetworkService struct {
	Generator *Generator
	Detector  BackendDetector
	Runner    IfupdownRunner
	Links     LinkSource
	StateDir  string
	ProcRoute string
	Window    time.Duration
	Now       func() time.Time

	mu    sync.Mutex
	timer *time.Timer
}

func (s *NetworkService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *NetworkService) window() time.Duration {
	if s.Window > 0 {
		return s.Window
	}
	return ConfirmWindow
}

func (s *NetworkService) pendingPath() string {
	return filepath.Join(s.StateDir, "network-pending.json")
}

func (s *NetworkService) routePath() string {
	if s.ProcRoute != "" {
		return s.ProcRoute
	}
	return "/proc/net/route"
}

// Recover restores a pending unconfirmed change left by a previous
// process. It is safe to call when there is nothing pending.
func (s *NetworkService) Recover(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.loadPending()
	if err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	return s.revertLocked(ctx, p)
}

// Close stops an in-flight confirm timer. The pending file is left for
// Recover on the next start if Confirm never ran.
func (s *NetworkService) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopTimerLocked()
}

// Status is the current backend, interfaces and any confirm window.
func (s *NetworkService) Status(ctx context.Context) (NetworkStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked(ctx)
}

func (s *NetworkService) statusLocked(ctx context.Context) (NetworkStatus, error) {
	detectCtx, cancel := detectTimeout(ctx)
	defer cancel()
	backend, reason, err := s.Detector.Detect(detectCtx)
	if err != nil {
		return NetworkStatus{}, fmt.Errorf("config: detecting network backend: %w", err)
	}
	st := NetworkStatus{
		Backend:        backend,
		Editable:       backend == BackendIfupdown,
		ReadOnlyReason: reason,
	}
	ifaces, err := s.Links.List(ctx)
	if err != nil {
		return NetworkStatus{}, fmt.Errorf("config: listing interfaces: %w", err)
	}
	managed := map[string]Iface{}
	if s.Generator != nil {
		raw, err := os.ReadFile(filepath.Join(s.Generator.Root, PathIfupdown))
		if err == nil {
			for _, live := range ifaces {
				if cfg, ok := parseManagedStanza(raw, live.Name); ok {
					managed[live.Name] = cfg
				}
			}
		} else if !os.IsNotExist(err) {
			return NetworkStatus{}, fmt.Errorf("config: reading %s: %w", PathIfupdown, err)
		}
	}
	root := ""
	if s.Generator != nil {
		root = s.Generator.Root
	}
	gwIface, gw := defaultGateway(s.routePath())
	st.Interfaces = mergeLive(ifaces, managed, defaultDNS(root), gwIface, gw)

	p, err := s.loadPending()
	if err != nil {
		return NetworkStatus{}, err
	}
	if p != nil {
		remaining := int(p.ExpiresAt.Sub(s.now()).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		st.Pending = &PendingChange{
			Interface:        p.Interface,
			ExpiresAt:        p.ExpiresAt.UTC(),
			RemainingSeconds: remaining,
		}
	}
	return st, nil
}

// Apply writes the managed ifupdown file, applies it, and starts the
// confirm-or-revert window.
func (s *NetworkService) Apply(ctx context.Context, change NetworkChange) (NetworkStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := s.statusLocked(ctx)
	if err != nil {
		return NetworkStatus{}, err
	}
	if !st.Editable {
		return NetworkStatus{}, fmt.Errorf("%w: %s", ErrNetworkReadOnly, st.ReadOnlyReason)
	}
	existing, err := s.loadPending()
	if err != nil {
		return NetworkStatus{}, err
	}
	if existing != nil {
		return NetworkStatus{}, ErrNetworkPending
	}

	body, err := RenderIfupdown(change)
	if err != nil {
		return NetworkStatus{}, err
	}

	prev, existed, err := s.readManaged()
	if err != nil {
		return NetworkStatus{}, err
	}
	if err := s.Generator.Write(ctx, File{Path: PathIfupdown, Command: "network", Body: []byte(body)}, 1, s.now()); err != nil {
		return NetworkStatus{}, err
	}
	expires := s.now().Add(s.window())
	pending := pendingFile{
		Interface:       change.Interface,
		PreviousBody:    prev,
		PreviousExisted: existed,
		ExpiresAt:       expires.UTC(),
	}
	if err := s.savePending(pending); err != nil {
		_ = s.restoreManaged(ctx, pending)
		return NetworkStatus{}, err
	}
	if err := s.Runner.Apply(ctx, change.Interface); err != nil {
		_ = s.revertLocked(ctx, &pending)
		return NetworkStatus{}, err
	}
	s.startTimerLocked()
	return s.statusLocked(ctx)
}

// Confirm keeps the new configuration. It must be called over that
// configuration — an unreachable address never reaches this method.
func (s *NetworkService) Confirm(ctx context.Context) (NetworkStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.loadPending()
	if err != nil {
		return NetworkStatus{}, err
	}
	if p == nil || !s.now().Before(p.ExpiresAt) {
		if p != nil {
			_ = s.revertLocked(ctx, p)
		}
		return NetworkStatus{}, ErrNetworkConfirmExpired
	}
	s.stopTimerLocked()
	if err := os.Remove(s.pendingPath()); err != nil && !os.IsNotExist(err) {
		return NetworkStatus{}, fmt.Errorf("config: clearing pending network change: %w", err)
	}
	return s.statusLocked(ctx)
}

func (s *NetworkService) startTimerLocked() {
	s.stopTimerLocked()
	delay := s.window()
	s.timer = time.AfterFunc(delay, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		p, err := s.loadPending()
		if err != nil || p == nil {
			return
		}
		_ = s.revertLocked(ctx, p)
	})
}

func (s *NetworkService) stopTimerLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

func (s *NetworkService) revertLocked(ctx context.Context, p *pendingFile) error {
	s.stopTimerLocked()
	if err := s.restoreManaged(ctx, *p); err != nil {
		return err
	}
	if err := os.Remove(s.pendingPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("config: clearing pending network change: %w", err)
	}
	return s.Runner.Apply(ctx, p.Interface)
}

func (s *NetworkService) readManaged() ([]byte, bool, error) {
	full := filepath.Join(s.Generator.Root, PathIfupdown)
	raw, err := os.ReadFile(full)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("config: reading %s: %w", PathIfupdown, err)
	}
	return raw, true, nil
}

func (s *NetworkService) restoreManaged(ctx context.Context, p pendingFile) error {
	if p.PreviousExisted {
		return s.Generator.restoreFile(ctx, PathIfupdown, p.PreviousBody)
	}
	_, err := s.Generator.RemoveManaged(ctx, PathIfupdown)
	return err
}

func (s *NetworkService) loadPending() (*pendingFile, error) {
	raw, err := os.ReadFile(s.pendingPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: reading pending network change: %w", err)
	}
	var p pendingFile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("config: parsing pending network change: %w", err)
	}
	return &p, nil
}

func (s *NetworkService) savePending(p pendingFile) error {
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return fmt.Errorf("config: creating network state dir: %w", err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("config: encoding pending network change: %w", err)
	}
	return atomicWrite(s.pendingPath(), raw, 0o600, false)
}

func (g *Generator) restoreFile(ctx context.Context, path string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, key, err := g.resolvePath(path)
	if err != nil {
		return err
	}
	if err := atomicWrite(full, content, 0o644, false); err != nil {
		return err
	}
	manifest, err := g.loadManifest()
	if err != nil {
		return err
	}
	rec := manifest[key]
	rec.Hash = hashContent(content)
	manifest[key] = rec
	return g.saveManifest(manifest)
}
