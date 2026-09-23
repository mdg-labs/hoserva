package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
)

func errSharesNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "shares are not configured on this daemon"}
}

func mapShareError(err error) error {
	switch {
	case errors.Is(err, store.ErrShareNotFound):
		return &apiError{code: "share_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, store.ErrShareExists):
		return &apiError{code: "share_exists", statusCode: 409, message: err.Error()}
	case errors.Is(err, share.ErrInvalidName), errors.Is(err, share.ErrInvalidInput), errors.Is(err, share.ErrPathEscapes):
		return &apiError{code: "share_invalid_input", statusCode: 400, message: err.Error()}
	case errors.Is(err, share.ErrConfirmation):
		return errConfirmRequired
	case errors.Is(err, share.ErrWrongSharePath):
		return &apiError{code: "share_wrong_path", statusCode: 400, message: err.Error()}
	case errors.Is(err, share.ErrFileNotFound):
		return &apiError{code: "share_file_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, share.ErrNoArray), errors.Is(err, store.ErrNoArray):
		return &apiError{code: "no_array", statusCode: 409, message: err.Error()}
	case errors.Is(err, config.ErrUnmanaged), errors.Is(err, config.ErrExistingHostFile):
		return &apiError{code: "unmanaged_config", statusCode: 409, message: err.Error()}
	default:
		return err
	}
}

func (h *Handler) ListShares(ctx context.Context) (*apiv1.ListSharesOK, error) {
	if h.Shares == nil {
		return nil, errSharesNotConfigured()
	}
	list, err := h.Shares.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing shares: %w", err)
	}
	out := make([]apiv1.Share, 0, len(list))
	for _, s := range list {
		out = append(out, shareToAPI(s))
	}
	return &apiv1.ListSharesOK{Shares: out}, nil
}

func (h *Handler) GetShare(ctx context.Context, params apiv1.GetShareParams) (*apiv1.Share, error) {
	if h.Shares == nil {
		return nil, errSharesNotConfigured()
	}
	s, err := h.Shares.Get(ctx, string(params.Name))
	if err != nil {
		return nil, mapShareError(err)
	}
	out := shareToAPI(s)
	return &out, nil
}

// admitShareMutation refuses a share mutation once the array is stopping
// or stopped (Q70, #333), and holds array stop until this call finishes.
// Create, update, delete, and deleting a share's files all mkdir or
// remove under disk mountpoints. A check of InMaintenance alone does
// not cover the gap before StopArray unmounts: the directory would sit
// on the root filesystem, and the next array start would hide it.
// Delete of an absent branch while the disk is unmounted would also
// report success and leave the data on the disk.
func (h *Handler) admitShareMutation() (func(), error) {
	if h.Scheduler == nil {
		return func() {}, nil
	}
	if err := h.Scheduler.BeginShareMutation(); err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return h.Scheduler.FinishShareMutation, nil
}

func (h *Handler) CreateShare(ctx context.Context, req *apiv1.CreateShareRequest) (*apiv1.Share, error) {
	if h.Shares == nil {
		return nil, errSharesNotConfigured()
	}
	release, err := h.admitShareMutation()
	if err != nil {
		return nil, err
	}
	defer release()
	in := share.CreateInput{Name: string(req.Name)}
	if v, ok := req.CacheMode.Get(); ok {
		in.CacheMode = pool.CacheMode(v)
	}
	if v, ok := req.CreatePolicy.Get(); ok {
		in.CreatePolicy = pool.CreatePolicy(v)
	}
	if v, ok := req.Smb.Get(); ok {
		smb := smbFromAPI(v)
		in.SMB = &smb
	}
	if v, ok := req.Nfs.Get(); ok {
		nfs := nfsFromAPI(v)
		in.NFS = &nfs
	}
	s, err := h.Shares.Create(ctx, in)
	if err != nil {
		return nil, mapShareError(err)
	}
	out := shareToAPI(s)
	return &out, nil
}

func (h *Handler) UpdateShare(ctx context.Context, req *apiv1.UpdateShareRequest, params apiv1.UpdateShareParams) (*apiv1.Share, error) {
	if h.Shares == nil {
		return nil, errSharesNotConfigured()
	}
	release, err := h.admitShareMutation()
	if err != nil {
		return nil, err
	}
	defer release()
	in := share.UpdateInput{}
	if v, ok := req.CacheMode.Get(); ok {
		mode := pool.CacheMode(v)
		in.CacheMode = &mode
	}
	if v, ok := req.CreatePolicy.Get(); ok {
		p := pool.CreatePolicy(v)
		in.CreatePolicy = &p
	}
	if v, ok := req.Smb.Get(); ok {
		smb := smbFromAPI(v)
		in.SMB = &smb
	}
	if v, ok := req.Nfs.Get(); ok {
		nfs := nfsFromAPI(v)
		in.NFS = &nfs
	}
	s, err := h.Shares.Update(ctx, string(params.Name), in)
	if err != nil {
		return nil, mapShareError(err)
	}
	out := shareToAPI(s)
	return &out, nil
}

func (h *Handler) DeleteShare(ctx context.Context, req *apiv1.ConfirmShareRequest, params apiv1.DeleteShareParams) error {
	if h.Shares == nil {
		return errSharesNotConfigured()
	}
	release, err := h.admitShareMutation()
	if err != nil {
		return err
	}
	defer release()
	if err := h.Shares.Delete(ctx, string(params.Name), req.Confirm); err != nil {
		return mapShareError(err)
	}
	return nil
}

func (h *Handler) DeleteShareData(ctx context.Context, req *apiv1.DeleteShareDataRequest, params apiv1.DeleteShareDataParams) error {
	if h.Shares == nil {
		return errSharesNotConfigured()
	}
	release, err := h.admitShareMutation()
	if err != nil {
		return err
	}
	defer release()
	if err := h.Shares.DeleteData(ctx, string(params.Name), req.Confirmation); err != nil {
		return mapShareError(err)
	}
	return nil
}

// StartShareRelocation queues a share_relocation job moving name's files
// between its cache path and the array (doc 09 §2, #54, #239) — the same
// startX shape StartSync/StartScrub/StartFix/StartMover already use,
// scoped to one share. RelocateToArray/RelocateToCache are never called
// from anywhere else (no second relocation-invocation path).
func (h *Handler) StartShareRelocation(ctx context.Context, req *apiv1.StartShareRelocationRequest, params apiv1.StartShareRelocationParams) (*apiv1.Job, error) {
	if h.Shares == nil {
		return nil, errSharesNotConfigured()
	}
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	if _, err := h.Shares.Get(ctx, string(params.Name)); err != nil {
		return nil, mapShareError(err)
	}
	encoded, err := json.Marshal(job.ShareRelocationParams{
		Share: string(params.Name),
		To:    string(req.To),
	})
	if err != nil {
		return nil, fmt.Errorf("encoding share relocation params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeShareRelocation, nil, encoded)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

func (h *Handler) BrowseShare(ctx context.Context, params apiv1.BrowseShareParams) (*apiv1.ShareBrowseResult, error) {
	if h.Shares == nil {
		return nil, errSharesNotConfigured()
	}
	rel := ""
	if v, ok := params.Path.Get(); ok {
		rel = v
	}
	listed, entries, err := h.Shares.Browse(ctx, string(params.Name), rel)
	if err != nil {
		return nil, mapShareError(err)
	}
	out := make([]apiv1.ShareBrowseEntry, 0, len(entries))
	for _, e := range entries {
		item := apiv1.ShareBrowseEntry{Name: e.Name}
		if e.Directory {
			item.Type = apiv1.ShareBrowseEntryTypeDirectory
		} else {
			item.Type = apiv1.ShareBrowseEntryTypeFile
			item.SizeBytes = apiv1.NewOptInt64(e.SizeBytes)
		}
		if e.Disk != "" {
			item.Disk = apiv1.NewOptString(e.Disk)
		}
		out = append(out, item)
	}
	return &apiv1.ShareBrowseResult{Path: listed, Entries: out}, nil
}

func (h *Handler) DeleteShareFile(ctx context.Context, req *apiv1.ConfirmShareRequest, params apiv1.DeleteShareFileParams) error {
	if h.Shares == nil {
		return errSharesNotConfigured()
	}
	release, err := h.admitShareMutation()
	if err != nil {
		return err
	}
	defer release()
	if err := h.Shares.DeleteFile(ctx, string(params.Name), params.Path, req.Confirm); err != nil {
		return mapShareError(err)
	}
	return nil
}

func shareToAPI(s share.Share) apiv1.Share {
	smb := apiv1.ShareSMB{
		Enabled:     s.SMB.Enabled,
		Guest:       s.SMB.Guest,
		ReadOnly:    s.SMB.ReadOnly,
		Browseable:  s.SMB.Browseable,
		Recycle:     s.SMB.Recycle,
		TimeMachine: s.SMB.TimeMachine,
	}
	if s.SMB.TimeMachineMaxSize != "" {
		smb.TimeMachineMaxSize = apiv1.NewOptNilString(s.SMB.TimeMachineMaxSize)
	}
	hosts := s.NFS.Hosts
	if hosts == nil {
		hosts = []string{}
	}
	squash := apiv1.ShareNFSSquash(s.NFS.Squash)
	if squash == "" {
		squash = apiv1.ShareNFSSquashRootSquash
	}
	return apiv1.Share{
		Name:         apiv1.ShareName(s.Name),
		Path:         s.Path(),
		CacheMode:    apiv1.ShareCacheMode(s.CacheMode),
		CreatePolicy: apiv1.ArrayCreatePolicy(s.CreatePolicy),
		Smb:          smb,
		Nfs: apiv1.ShareNFS{
			Enabled: s.NFS.Enabled,
			Hosts:   append([]string(nil), hosts...),
			Squash:  squash,
		},
		Usage:     shareUsageToAPI(s.Usage),
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
}

// shareUsageToAPI maps share.Usage to the API's nullable ShareUsage
// (doc 03 §4.1-4.2, #223): Null when the share has not been through a
// sync since it was created — never a zero value dressed up as real data.
func shareUsageToAPI(u share.Usage) apiv1.NilShareUsage {
	if !u.Synced {
		var nu apiv1.NilShareUsage
		nu.SetToNull()
		return nu
	}
	perDisk := make([]apiv1.ShareDiskUsage, 0, len(u.Disks))
	for disk, bytes := range u.Disks {
		perDisk = append(perDisk, apiv1.ShareDiskUsage{Disk: disk, Bytes: bytes})
	}
	sort.Slice(perDisk, func(i, j int) bool { return perDisk[i].Disk < perDisk[j].Disk })
	return apiv1.NewNilShareUsage(apiv1.ShareUsage{
		TotalBytes: u.TotalBytes,
		PerDisk:    perDisk,
		AsOf:       u.AsOf,
	})
}

func smbFromAPI(s apiv1.ShareSMB) share.SMB {
	out := share.SMB{
		Enabled:     s.Enabled,
		Guest:       s.Guest,
		ReadOnly:    s.ReadOnly,
		Browseable:  s.Browseable,
		Recycle:     s.Recycle,
		TimeMachine: s.TimeMachine,
	}
	if v, ok := s.TimeMachineMaxSize.Get(); ok {
		out.TimeMachineMaxSize = v
	}
	return out
}

func nfsFromAPI(s apiv1.ShareNFS) share.NFS {
	return share.NFS{
		Enabled: s.Enabled,
		Hosts:   append([]string(nil), s.Hosts...),
		Squash:  string(s.Squash),
	}
}
