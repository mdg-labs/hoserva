// Command nutconfig prints one of internal/config's own rendered NUT
// files (issue #250, doc 02 §6, Q77) for scripts/vm/ups-check.sh: the
// exact RenderNUTConf/RenderUPSConf/RenderUPSDUsers/RenderUPSMonConf
// output a running hoservad's own WriteUPS would write for a USB-attached
// UPS, with NUT's own dummy-ups driver named in place of a real one — the
// L3 dummy-ups harness configures NUT from the real generator, never a
// hand-rolled substitute of it. Run with `go run` from the repo root;
// compiling and running it touches no device, only internal/config's
// pure string rendering.
package main

import (
	"fmt"
	"os"

	"github.com/mdg-labs/hoserva/internal/config"
)

// dummyUPSState is the UPSState a real onboarding flow would produce for
// doc 03 §8.1's UPS settings card, pointed at NUT's own dummy-ups driver
// (D20: simulated hardware, never a real UPS) instead of a real USB
// device. Port names the dummy-ups definition file (dummy-ups(8)) this
// harness edits directly to drive the simulated battery state — that file
// is NUT's own simulated-hardware artifact, not a Hoserva-managed config,
// so it has no Render function of its own here.
var dummyUPSState = config.UPSState{
	Connection:        config.UPSConnectionUSB,
	Driver:            "dummy-ups",
	Port:              "hoserva-ups.dev",
	MonitorPassword:   "hoserva-l3-monitor-pass",
	LowBatteryPercent: 20,
	RuntimeSeconds:    300,
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: nutconfig <nut.conf|ups.conf|upsd.users|upsmon.conf>")
		os.Exit(2)
	}

	var out string
	switch os.Args[1] {
	case "nut.conf":
		out = config.RenderNUTConf(dummyUPSState)
	case "ups.conf":
		out = config.RenderUPSConf(dummyUPSState)
	case "upsd.users":
		out = config.RenderUPSDUsers(dummyUPSState)
	case "upsmon.conf":
		out = config.RenderUPSMonConf(dummyUPSState)
	default:
		fmt.Fprintf(os.Stderr, "nutconfig: unknown file %q\n", os.Args[1])
		os.Exit(2)
	}
	fmt.Print(out)
}
