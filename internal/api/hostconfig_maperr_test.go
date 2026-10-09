package api

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mdg-labs/hoserva/internal/config"
)

func TestMapDockerDataRootErrReportsAnOccupiedDirectoryAs409(t *testing.T) {
	err := mapDockerDataRootErr(fmt.Errorf("checking docker data-root move: %w", config.ErrDockerDataRootInUse))
	var ae *apiError
	if !errors.As(err, &ae) || ae.statusCode != 409 || ae.code != "docker_data_root_in_use" {
		t.Fatalf("mapDockerDataRootErr() = %#v, want a 409 docker_data_root_in_use", err)
	}
}
