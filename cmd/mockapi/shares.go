package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func errShareNotFound(name apiv1.ShareName) error {
	return &mockError{code: "share_not_found", statusCode: 404, message: fmt.Sprintf("no share named %s", name)}
}

func errShareExists(name apiv1.ShareName) error {
	return &mockError{code: "share_exists", statusCode: 409, message: fmt.Sprintf("share %s already exists", name)}
}

func defaultShareNFS() apiv1.ShareNFS {
	return apiv1.ShareNFS{
		Enabled: false,
		Hosts:   []string{},
		Squash:  apiv1.ShareNFSSquashRootSquash,
	}
}

func normalizeShareNFS(n apiv1.ShareNFS) apiv1.ShareNFS {
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
	name := string(req.Name)
	if _, ok := h.shares[name]; ok {
		return nil, errShareExists(req.Name)
	}
	mode := apiv1.ShareCacheModeCacheThenMove
	if v, ok := req.CacheMode.Get(); ok {
		mode = v
	}
	policy := apiv1.ArrayCreatePolicyMspmfs
	if v, ok := req.CreatePolicy.Get(); ok {
		policy = v
	}
	smb := apiv1.ShareSMB{Enabled: true, Browseable: true}
	if v, ok := req.Smb.Get(); ok {
		smb = v
	}
	nfs := defaultShareNFS()
	if v, ok := req.Nfs.Get(); ok {
		nfs = normalizeShareNFS(v)
	}
	now := time.Now().UTC()
	s := apiv1.Share{
		Name:         req.Name,
		Path:         "/mnt/user/" + name,
		CacheMode:    mode,
		CreatePolicy: policy,
		Smb:          smb,
		Nfs:          nfs,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	h.shares[name] = s
	return &s, nil
}

func (h *handler) UpdateShare(ctx context.Context, req *apiv1.UpdateShareRequest, params apiv1.UpdateShareParams) (*apiv1.Share, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.shares[string(params.Name)]
	if !ok {
		return nil, errShareNotFound(params.Name)
	}
	if v, ok := req.CacheMode.Get(); ok {
		s.CacheMode = v
	}
	if v, ok := req.CreatePolicy.Get(); ok {
		s.CreatePolicy = v
	}
	if v, ok := req.Smb.Get(); ok {
		s.Smb = v
	}
	if v, ok := req.Nfs.Get(); ok {
		s.Nfs = normalizeShareNFS(v)
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
	if _, ok := h.shares[string(params.Name)]; !ok {
		return errShareNotFound(params.Name)
	}
	return nil
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
