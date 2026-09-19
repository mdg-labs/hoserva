package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mdg-labs/hoserva/internal/parity"
)

const snapraidConfRelPath = "snapraid.conf"

// newSnapraidEngine constructs the daemon's parity.SnapraidEngine when a
// generated snapraid.conf exists under configRoot (D4). No config yet
// returns nil so GET /parity and POST /parity/diff stay 501 rather than
// shelling out to snapraid against a missing file. Permission and other
// Stat errors are returned so the daemon does not treat an unreadable
// config as "not configured".
func newSnapraidEngine(configRoot, stateDir string, runner parity.Runner) (*parity.SnapraidEngine, error) {
	confPath := filepath.Join(configRoot, snapraidConfRelPath)
	if _, err := os.Stat(confPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", confPath, err)
	}
	return &parity.SnapraidEngine{
		ConfPath: confPath,
		LogDir:   filepath.Join(stateDir, "snapraid"),
		Runner:   runner,
	}, nil
}
