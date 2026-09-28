package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/store"
)

func errInvalidHostConfig(msg string) error {
	return &apiError{code: "invalid_host_config", statusCode: 400, message: msg}
}

// mapDockerDataRootErr maps a Q76 data-root refusal to the same 409
// unmanaged_config other host-config handlers already use for
// config.ErrUnmanaged/config.ErrExistingHostFile (share_handler.go,
// network_handler.go, ups_handler.go) — an existing, unmanaged
// /etc/docker/daemon.json must be reported, never turned into an opaque
// 500 by the generic error path.
func mapDockerDataRootErr(err error) error {
	if errors.Is(err, config.ErrUnmanaged) || errors.Is(err, config.ErrExistingHostFile) {
		return &apiError{code: "unmanaged_config", statusCode: 409, message: err.Error()}
	}
	return err
}

func (h *Handler) ApplyHostConfig(ctx context.Context, req *apiv1.ApplyHostConfigRequest) (*apiv1.ApplyHostConfigResult, error) {
	if h.Generator == nil || h.HostConfig == nil {
		return nil, &apiError{code: "not_configured", statusCode: 501, message: "host-config apply is not configured on this daemon"}
	}

	inv, err := config.Detect(ctx, h.Generator.Root, h.Docker)
	if err != nil {
		return nil, fmt.Errorf("detecting host configuration: %w", err)
	}

	type pendingChoice struct {
		choice   apiv1.HostConfigChoice
		kind     string
		decision string
		facts    string
		path     string
	}

	seen := map[string]struct{}{}
	pending := make([]pendingChoice, 0, len(req.Files))
	for _, choice := range req.Files {
		kind, ok := config.KindFromCheckID(string(choice.ID))
		if !ok {
			return nil, errInvalidHostConfig(fmt.Sprintf("unknown host-config id %q", choice.ID))
		}
		if _, dup := seen[kind]; dup {
			return nil, errInvalidHostConfig(fmt.Sprintf("duplicate choice for %s", choice.ID))
		}
		seen[kind] = struct{}{}
		if !inv.Found(kind) {
			return nil, errInvalidHostConfig(fmt.Sprintf("%s was not detected on this host", choice.ID))
		}

		decision := string(choice.Decision)
		if decision != config.DecisionImport && decision != config.DecisionLeave {
			return nil, errInvalidHostConfig(fmt.Sprintf("decision for %s must be import or leave", choice.ID))
		}

		facts, err := config.FactsJSON(inv, kind)
		if err != nil {
			return nil, fmt.Errorf("encoding host-config facts: %w", err)
		}
		pending = append(pending, pendingChoice{
			choice:   choice,
			kind:     kind,
			decision: decision,
			facts:    string(facts),
			path:     config.HostFilePath(kind),
		})
	}

	files := make([]config.HostFileDecision, 0, len(pending))
	rows := make([]store.HostConfig, 0, len(pending))
	applied := make([]apiv1.HostConfigChoice, 0, len(pending))
	importDocker := map[string]bool{}
	var importSamba, importNFS bool
	for _, p := range pending {
		if p.path != "" {
			files = append(files, config.HostFileDecision{Path: p.path, Decision: p.decision})
		}
		rows = append(rows, store.HostConfig{Kind: p.kind, Decision: p.decision, Facts: p.facts})
		if p.kind == config.KindDockerContainers || p.kind == config.KindDockerImages {
			importDocker[p.kind] = p.decision == config.DecisionImport
		}
		if p.decision == config.DecisionImport {
			switch p.kind {
			case config.KindSamba:
				importSamba = true
			case config.KindNFS:
				importNFS = true
			}
		}
		applied = append(applied, p.choice)
	}

	// The data-root move's own preflight runs before anything else in this
	// request is committed (finding: an existing, unmanaged
	// /etc/docker/daemon.json must never be discovered only after the
	// Samba import, host-file decisions and host-config rows below are
	// already durably persisted — every retry would hit the same refusal
	// with those already applied). acceptedMove/dataRoot are pure — no I/O
	// beyond the read-only hasCacheDisk lookup — so computing them this
	// early costs nothing even when the request carries no Docker choices.
	acceptedMove := importDocker[config.KindDockerContainers] && importDocker[config.KindDockerImages]
	dataRoot := config.DockerDataRoot(inv, acceptedMove, h.hasCacheDisk(ctx))
	if err := h.Generator.CanApplyDockerDataRoot(ctx, dataRoot); err != nil {
		return nil, mapDockerDataRootErr(fmt.Errorf("checking docker data-root move: %w", err))
	}

	var insertedShares []string
	if importSamba || importNFS {
		if h.Shares == nil {
			return nil, &apiError{code: "not_configured", statusCode: 501, message: "share import is not configured on this daemon"}
		}
		if importSamba {
			if err := h.Generator.EnsureSambaCustomConf(); err != nil {
				return nil, fmt.Errorf("ensuring smb.custom.conf: %w", err)
			}
		}
		var sambaRaw, nfsRaw []byte
		if importSamba {
			sambaRaw, err = os.ReadFile(filepath.Join(h.Generator.Root, config.PathSamba))
			if err != nil {
				return nil, fmt.Errorf("reading smb.conf for import: %w", err)
			}
		}
		if importNFS {
			nfsRaw, err = os.ReadFile(filepath.Join(h.Generator.Root, config.PathNFS))
			if err != nil {
				return nil, fmt.Errorf("reading exports for import: %w", err)
			}
		}
		insertedShares, err = h.Shares.ImportFromHost(ctx, sambaRaw, nfsRaw)
		if err != nil {
			return nil, fmt.Errorf("importing host shares: %w", err)
		}
	}

	restore, err := h.Generator.ApplyHostFileDecisions(ctx, files)
	if err != nil {
		if rbErr := h.rollbackImportedShares(ctx, insertedShares); rbErr != nil {
			return nil, fmt.Errorf("recording host-file decisions: %w (rolling back imported shares: %v)", err, rbErr)
		}
		return nil, fmt.Errorf("recording host-file decisions: %w", err)
	}
	if err := h.HostConfig.PutAll(ctx, rows); err != nil {
		if restore != nil {
			if rerr := restore(); rerr != nil {
				if rbErr := h.rollbackImportedShares(ctx, insertedShares); rbErr != nil {
					return nil, fmt.Errorf("persisting host-config: %w (manifest restore: %v; rolling back imported shares: %v)", err, rerr, rbErr)
				}
				return nil, fmt.Errorf("persisting host-config: %w (manifest restore: %v)", err, rerr)
			}
		}
		if rbErr := h.rollbackImportedShares(ctx, insertedShares); rbErr != nil {
			return nil, fmt.Errorf("persisting host-config: %w (rolling back imported shares: %v)", err, rbErr)
		}
		return nil, fmt.Errorf("persisting host-config: %w", err)
	}

	// ApplyDockerDataRoot performs the move the preflight above already
	// cleared (Q62, Q76): the host-config rows are already durably
	// persisted by this point, so a failure here is reported rather than
	// swallowed — dockerDataRoot in the response must reflect what was
	// actually written, never the decision alone, and the caller can
	// safely retry (Write is idempotent per path). Mapped through the same
	// 409 as the preflight for defense in depth against a daemon.json that
	// appeared in the window between the two calls; the preflight above is
	// what keeps that window from mattering in the common case.
	if err := h.Generator.ApplyDockerDataRoot(ctx, dataRoot, h.DockerDirs, h.DockerRestart, 1, time.Now()); err != nil {
		return nil, mapDockerDataRootErr(fmt.Errorf("applying docker data-root: %w", err))
	}
	return &apiv1.ApplyHostConfigResult{Files: applied, DockerDataRoot: dataRoot}, nil
}

func (h *Handler) rollbackImportedShares(ctx context.Context, names []string) error {
	if h.Shares == nil || h.Shares.Shares == nil || len(names) == 0 {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	var first error
	deleted := false
	for i := len(names) - 1; i >= 0; i-- {
		if err := h.Shares.Shares.Delete(ctx, names[i]); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		deleted = true
	}
	if deleted {
		h.Shares.RefreshSnapshot(ctx)
	}
	return first
}

func (h *Handler) hasCacheDisk(ctx context.Context) bool {
	if h.ArrayStore == nil {
		return false
	}
	_, disks, err := h.ArrayStore.GetArray(ctx)
	if err != nil {
		return false
	}
	for _, d := range disks {
		if d.Role == store.ArrayRoleCache {
			return true
		}
	}
	return false
}

func hostConfigChecks(inv *config.HostInventory) []apiv1.DoctorCheck {
	if inv == nil {
		return nil
	}
	var checks []apiv1.DoctorCheck
	checks = appendFileCheck(checks, "host_samba", "Samba shares", "share", inv.Samba)
	checks = appendFileCheck(checks, "host_nfs", "NFS exports", "export", inv.NFS)
	checks = appendFileCheck(checks, "host_fstab", "fstab mounts", "mount", inv.Fstab)
	if inv.Found(config.KindDockerContainers) {
		checks = append(checks, dockerInventoryCheck("host_docker_containers", "Docker containers", "container", inv.DockerContainers, inv.DockerErr))
	}
	if inv.Found(config.KindDockerImages) {
		checks = append(checks, dockerInventoryCheck("host_docker_images", "Docker images", "image", inv.DockerImages, inv.DockerErr))
	}
	// host_docker_volumes, host_docker_networks and host_docker_plugins have
	// no import/leave decision — there is no host file for any of them to
	// manage — so unlike containers/images above they are reported only
	// when there is something to say: a listing failure (fails closed, same
	// as dockerInventoryCheck) or an actual entry that blocks the cache
	// data-root move (Q76, #413, #416). An empty, successfully-listed
	// category is omitted entirely rather than reported as "0 found" next
	// to a reason text that would not be true. They share the
	// container/image checks' "docker is available" gate rather than
	// config.KindFromCheckID/Found, since none of them is ever a
	// submittable HostConfigID.
	if !inv.DockerUnavailable {
		checks = appendDockerInfoCheck(checks, "host_docker_volumes", "Docker volumes", "volume", inv.DockerVolumes, inv.DockerErr)
		checks = appendDockerInfoCheck(checks, "host_docker_networks", "Docker networks", "network", inv.DockerNetworks, inv.DockerErr)
		checks = appendDockerInfoCheck(checks, "host_docker_plugins", "Docker plugins", "plugin", inv.DockerPlugins, inv.DockerErr)
	}
	return checks
}

func appendDockerInfoCheck(checks []apiv1.DoctorCheck, id, name, unit string, refs []config.DockerRef, probeErr error) []apiv1.DoctorCheck {
	if probeErr == nil && len(refs) == 0 {
		return checks
	}
	return append(checks, dockerInventoryCheck(id, name, unit, refs, probeErr))
}

func appendFileCheck(checks []apiv1.DoctorCheck, id, name, unit string, file config.HostFile) []apiv1.DoctorCheck {
	if !file.Present && file.Err == nil {
		return checks
	}
	if file.Err != nil {
		return append(checks, apiv1.DoctorCheck{
			ID:      id,
			Name:    name,
			Status:  apiv1.DoctorCheckStatusWarn,
			Message: fmt.Sprintf("Could not read %s: %v", file.Path, file.Err),
		})
	}
	msg := fmt.Sprintf("%s is present at %s", name, file.Path)
	if len(file.Items) > 0 {
		msg = fmt.Sprintf("%d %s(s): %s", len(file.Items), unit, strings.Join(file.Items, ", "))
	}
	return append(checks, apiv1.DoctorCheck{
		ID:      id,
		Name:    name,
		Status:  apiv1.DoctorCheckStatusPass,
		Message: msg,
	})
}

func dockerInventoryCheck(id, name, unit string, refs []config.DockerRef, probeErr error) apiv1.DoctorCheck {
	if probeErr != nil {
		return apiv1.DoctorCheck{
			ID:      id,
			Name:    name,
			Status:  apiv1.DoctorCheckStatusWarn,
			Message: fmt.Sprintf("Could not list %s: %v", name, probeErr),
		}
	}
	labels := make([]string, 0, len(refs))
	for _, r := range refs {
		labels = append(labels, r.Name)
	}
	return apiv1.DoctorCheck{
		ID:      id,
		Name:    name,
		Status:  apiv1.DoctorCheckStatusPass,
		Message: fmt.Sprintf("%d %s(s): %s", len(refs), unit, strings.Join(labels, ", ")),
	}
}
