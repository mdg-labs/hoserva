package main

import (
	"os"
	"path/filepath"

	"github.com/mdg-labs/hoserva/internal/parity"
)

const snapraidConfRelPath = "snapraid.conf"

// newSnapraidEngine constructs the daemon's parity.SnapraidEngine when a
// generated snapraid.conf exists under configRoot (D4). No config yet
// returns nil so GET /parity and POST /parity/diff stay 501 rather than
// shelling out to snapraid against a missing file.
func newSnapraidEngine(configRoot, stateDir string, runner parity.Runner) *parity.SnapraidEngine {
	confPath := filepath.Join(configRoot, snapraidConfRelPath)
	if _, err := os.Stat(confPath); err != nil {
		return nil
	}
	return &parity.SnapraidEngine{
		ConfPath: confPath,
		LogDir:   filepath.Join(stateDir, "snapraid"),
		Runner:   runner,
	}
}
