package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/store"
)

func errShareNotFound(name apiv1.ShareName) error {
	return &mockError{code: "share_not_found", statusCode: 404, message: fmt.Sprintf("no share named %s", name)}
}

func errShareExists(name apiv1.ShareName) error {
	return &mockError{code: "share_exists", statusCode: 409, message: fmt.Sprintf("share %s already exists", name)}
}

func errShareInvalidInput(msg string) error {
	return &mockError{code: "share_invalid_input", statusCode: 400, message: msg}
}

// mockCacheModeNeedsCacheDisk mirrors internal/share.Service.Create/
// Update's own cache-disk check: cache-then-move and cache-only both need
// a cache disk in the array, and mockArrayDisks("healthy") has none, so a
// share created with no explicit cache mode (production's own default is
// cache-then-move, internal/share/service.go) must be refused the same
// way production refuses it, not silently accepted. A nil-array scenario
// (fresh-install) is left alone here: production refuses share creation
// there for a different reason first (ErrNoArray, "no_array") that this
// mock does not model at all yet — a pre-existing gap this check must not
// paper over by reporting the wrong code.
func mockCacheModeNeedsCacheDisk(mode apiv1.ShareCacheMode, scenario string) error {
	if mode != apiv1.ShareCacheModeCacheThenMove && mode != apiv1.ShareCacheModeCacheOnly {
		return nil
	}
	disks := mockArrayDisks(scenario)
	if disks == nil {
		return nil
	}
	for _, d := range disks {
		if d.Role == store.ArrayRoleCache {
			return nil
		}
	}
	return errShareInvalidInput(fmt.Sprintf("cache mode %q needs a cache disk", mode))
}

// defaultShareNFS and normalizeShareNFS both stamp fsid from name the
// same way internal/api's shareToAPI does (#351), so the mock's
// response matches what a real hoservad would return for the same
// share name.
func defaultShareNFS(name string) apiv1.ShareNFS {
	return apiv1.ShareNFS{
		Enabled: false,
		Hosts:   []string{},
		Squash:  apiv1.ShareNFSSquashRootSquash,
		Fsid:    apiv1.NewOptUUID(config.NFSExportFsid(name)),
	}
}

func normalizeShareNFS(name string, n apiv1.ShareNFS) apiv1.ShareNFS {
	hosts := n.Hosts
	if hosts == nil {
		hosts = []string{}
	}
	squash := n.Squash
	if squash == "" {
		squash = apiv1.ShareNFSSquashRootSquash
	}
	return apiv1.ShareNFS{
		Enabled: n.Enabled,
		Hosts:   append([]string(nil), hosts...),
		Squash:  squash,
		Fsid:    apiv1.NewOptUUID(config.NFSExportFsid(name)),
	}
}

func (h *handler) ListShares(ctx context.Context) (*apiv1.ListSharesOK, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]apiv1.Share, 0, len(h.shares))
	for _, s := range h.shares {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return &apiv1.ListSharesOK{Shares: out}, nil
}

func (h *handler) GetShare(ctx context.Context, params apiv1.GetShareParams) (*apiv1.Share, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.shares[string(params.Name)]
	if !ok {
		return nil, errShareNotFound(params.Name)
	}
	return &s, nil
}

func (h *handler) CreateShare(ctx context.Context, req *apiv1.CreateShareRequest) (*apiv1.Share, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maintenance {
		return nil, errMaintenanceMode()
	}
	name := string(req.Name)
	if _, ok := h.shares[name]; ok {
		return nil, errShareExists(req.Name)
	}
	mode := apiv1.ShareCacheModeCacheThenMove
	if v, ok := req.CacheMode.Get(); ok {
		mode = v
	}
	if err := mockCacheModeNeedsCacheDisk(mode, h.scenario); err != nil {
		return nil, err
	}
	policy := apiv1.ArrayCreatePolicyMspmfs
	if v, ok := req.CreatePolicy.Get(); ok {
		policy = v
	}
	smb := apiv1.ShareSMB{Enabled: true, Browseable: true}
	if v, ok := req.Smb.Get(); ok {
		smb = v
	}
	nfs := defaultShareNFS(name)
	if v, ok := req.Nfs.Get(); ok {
		nfs = normalizeShareNFS(name, v)
	}
	now := time.Now().UTC()
	s := apiv1.Share{
		Name:         req.Name,
		Path:         "/mnt/user/" + name,
		CacheMode:    mode,
		CreatePolicy: policy,
		Smb:          smb,
		Nfs:          nfs,
		Usage:        apiv1.NilShareUsage{Null: true},
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	h.shares[name] = s
	return &s, nil
}

func (h *handler) UpdateShare(ctx context.Context, req *apiv1.UpdateShareRequest, params apiv1.UpdateShareParams) (*apiv1.Share, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maintenance {
		return nil, errMaintenanceMode()
	}
	s, ok := h.shares[string(params.Name)]
	if !ok {
		return nil, errShareNotFound(params.Name)
	}
	if v, ok := req.CacheMode.Get(); ok {
		if err := mockCacheModeNeedsCacheDisk(v, h.scenario); err != nil {
			return nil, err
		}
		s.CacheMode = v
	}
	if v, ok := req.CreatePolicy.Get(); ok {
		s.CreatePolicy = v
	}
	if v, ok := req.Smb.Get(); ok {
		s.Smb = v
	}
	if v, ok := req.Nfs.Get(); ok {
		s.Nfs = normalizeShareNFS(string(params.Name), v)
	}
	s.UpdatedAt = time.Now().UTC()
	h.shares[string(params.Name)] = s
	return &s, nil
}

func (h *handler) DeleteShare(ctx context.Context, req *apiv1.ConfirmShareRequest, params apiv1.DeleteShareParams) error {
	if !req.Confirm {
		return errConfirmRequired()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maintenance {
		return errMaintenanceMode()
	}
	if _, ok := h.shares[string(params.Name)]; !ok {
		return errShareNotFound(params.Name)
	}
	delete(h.shares, string(params.Name))
	return nil
}

func (h *handler) DeleteShareData(ctx context.Context, req *apiv1.DeleteShareDataRequest, params apiv1.DeleteShareDataParams) error {
	if req.Confirmation != string(params.Name) {
		return errConfirmRequired()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maintenance {
		return errMaintenanceMode()
	}
	if _, ok := h.shares[string(params.Name)]; !ok {
		return errShareNotFound(params.Name)
	}
	return nil
}

func (h *handler) DeleteShareFile(ctx context.Context, req *apiv1.ConfirmShareRequest, params apiv1.DeleteShareFileParams) error {
	if !req.Confirm {
		return errConfirmRequired()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maintenance {
		return errMaintenanceMode()
	}
	if _, ok := h.shares[string(params.Name)]; !ok {
		return errShareNotFound(params.Name)
	}
	return nil
}

// StartShareRelocation is startShareRelocation's mock (doc 09 §2, #239) —
// the same TypeShareRelocation job a real daemon submits through
// RunShareRelocation; this mock has no scheduler of its own, so it just
// records the queued job the way StartMover does for TypeMover.
func (h *handler) StartShareRelocation(ctx context.Context, req *apiv1.StartShareRelocationRequest, params apiv1.StartShareRelocationParams) (*apiv1.Job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.shares[string(params.Name)]; !ok {
		return nil, errShareNotFound(params.Name)
	}
	// Production's Scheduler.Submit refuses every job type but
	// TypeDiskUpgradeData while maintenance mode is active (Q70).
	if h.maintenance {
		return nil, errMaintenanceMode()
	}
	now := time.Now().UTC()
	j := apiv1.Job{
		ID:          uuid.New(),
		Type:        apiv1.JobTypeShareRelocation,
		Class:       apiv1.JobClassArrayWrite,
		Status:      apiv1.JobStatusQueued,
		Resumable:   true,
		Cancellable: true,
		CreatedAt:   now,
	}
	h.jobs[j.ID] = j
	return &j, nil
}

func (h *handler) BrowseShare(ctx context.Context, params apiv1.BrowseShareParams) (*apiv1.ShareBrowseResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.shares[string(params.Name)]; !ok {
		return nil, errShareNotFound(params.Name)
	}
	rel := ""
	if v, ok := params.Path.Get(); ok {
		rel = v
	}
	return &apiv1.ShareBrowseResult{Path: rel, Entries: []apiv1.ShareBrowseEntry{}}, nil
}
