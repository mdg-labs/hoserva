package api

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/config"
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

func (h *Handler) CreateShare(ctx context.Context, req *apiv1.CreateShareRequest) (*apiv1.Share, error) {
	if h.Shares == nil {
		return nil, errSharesNotConfigured()
	}
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
	if err := h.Shares.Delete(ctx, string(params.Name), req.Confirm); err != nil {
		return mapShareError(err)
	}
	return nil
}

func (h *Handler) DeleteShareData(ctx context.Context, req *apiv1.DeleteShareDataRequest, params apiv1.DeleteShareDataParams) error {
	if h.Shares == nil {
		return errSharesNotConfigured()
	}
	if err := h.Shares.DeleteData(ctx, string(params.Name), req.Confirmation); err != nil {
		return mapShareError(err)
	}
	return nil
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
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
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
