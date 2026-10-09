package template

import (
	"testing"

	"github.com/mdg-labs/hoserva/internal/config"
)

func TestDockerDataRootConstantsMatchConfig(t *testing.T) {
	if dockerDataRoot != config.DockerDataRootCache {
		t.Errorf("dockerDataRoot = %q, config.DockerDataRootCache = %q", dockerDataRoot, config.DockerDataRootCache)
	}
	if legacyDockerDataRoot != config.DockerDataRootCacheLegacy {
		t.Errorf("legacyDockerDataRoot = %q, config.DockerDataRootCacheLegacy = %q", legacyDockerDataRoot, config.DockerDataRootCacheLegacy)
	}
}

func TestInsideLayoutRefusesDockerDataRootThroughThePool(t *testing.T) {
	for _, p := range []string{
		"/mnt/cache/.docker", "/mnt/cache/.docker/volumes", "/mnt/user/.docker", "/mnt/user/.docker/overlay2",
		"/mnt/cache/docker", "/mnt/user/docker", "/mnt/user/docker/containers", "/mnt/user/../user/.docker",
	} {
		if insideLayout(p) {
			t.Errorf("insideLayout(%q) = true, want false: it is or resolves to Docker's data-root", p)
		}
	}
	for _, p := range []string{"/mnt/user/media", "/mnt/cache/appdata/x", "/mnt/user/dockerish", "/mnt/user/.dockerish"} {
		if !insideLayout(p) {
			t.Errorf("insideLayout(%q) = false, want true", p)
		}
	}
}
