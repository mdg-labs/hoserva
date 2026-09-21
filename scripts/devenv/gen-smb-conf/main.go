// Command gen-smb-conf prints the smb.conf a single test share produces
// through internal/config.RenderSambaConf (issue #219), so the lab's Samba
// check (scripts/devenv/smb-check.sh) exercises Hoserva's real generator
// output rather than a hand-written fixture.
//
// It runs on the host, via `make test-integration`, before the lab
// container is asked to do anything — the lab image carries no Go
// toolchain, and rendering an smb.conf needs none of the container's own
// capabilities. Everything Samba-related still happens inside the lab
// container, in smb-check.sh.
package main

import (
	"fmt"
	"os"

	"github.com/mdg-labs/hoserva/internal/config"
)

func main() {
	shares := []config.SambaShare{
		{
			Name:       "smbcheck",
			Guest:      false,
			ReadOnly:   false,
			Browseable: true,
		},
	}
	if _, err := fmt.Fprint(os.Stdout, config.RenderSambaConf(shares)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "gen-smb-conf: write stdout:", err)
		os.Exit(1)
	}
}
