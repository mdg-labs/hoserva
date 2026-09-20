package api

import (
	"context"
	"fmt"
	"strings"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/store"
)

func errInvalidHostConfig(msg string) error {
	return &apiError{code: "invalid_host_config", statusCode: 400, message: msg}
}

func (h *Handler) ApplyHostConfig(ctx context.Context, req *apiv1.ApplyHostConfigRequest) (*apiv1.ApplyHostConfigResult, error) {
	if h.Generator == nil || h.HostConfig == nil {
		return nil, &apiError{code: "not_configured", statusCode: 501, message: "host-config apply is not configured on this daemon"}
	}

	inv, err := config.Detect(ctx, h.Generator.Root, h.Docker)
	if err != nil {
		return nil, fmt.Errorf("detecting host configuration: %w", err)
	}

	seen := map[string]struct{}{}
	applied := make([]apiv1.HostConfigChoice, 0, len(req.Files))
	importDocker := map[string]bool{}

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
		if err := h.HostConfig.Put(ctx, store.HostConfig{
			Kind:     kind,
			Decision: decision,
			Facts:    string(facts),
		}); err != nil {
			return nil, err
		}

		if path := config.HostFilePath(kind); path != "" {
			if decision == config.DecisionLeave {
				if err := h.Generator.KeepUnmanaged(ctx, path); err != nil {
					return nil, fmt.Errorf("leaving %s unmanaged: %w", path, err)
				}
			} else {
				if err := h.Generator.RecordImported(ctx, path); err != nil {
					return nil, fmt.Errorf("recording import of %s: %w", path, err)
				}
			}
		}
		if kind == config.KindDockerContainers || kind == config.KindDockerImages {
			importDocker[kind] = decision == config.DecisionImport
		}
		applied = append(applied, choice)
	}

	acceptedMove := importDocker[config.KindDockerContainers] && importDocker[config.KindDockerImages]
	dataRoot := config.DockerDataRoot(inv, acceptedMove, h.hasCacheDisk(ctx))
	return &apiv1.ApplyHostConfigResult{Files: applied, DockerDataRoot: dataRoot}, nil
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
	return checks
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
