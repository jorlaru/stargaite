// Package host is the console's access to the machine it runs on: the clone
// it was exported into, and the commands it runs there, docker compose and
// git. It is the console's only package that runs a command, and every
// command it runs is bounded and stopped with its whole process group when
// the console stops (OPS-11: every wait is bounded).
package host

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// localBound bounds each local read: docker compose ps and git describe.
const localBound = 20 * time.Second

// Host is the clone. Every command runs with its directory as the working
// directory.
type Host struct {
	Dir   string
	bound time.Duration // localBound; tests shorten the field, never the constant
}

// New returns the clone at dir.
func New(dir string) *Host { return &Host{Dir: dir, bound: localBound} }

// Find returns the clone the console was exported into: the parent of the
// executable's directory, bin/, which must hold compose.yaml.
func Find(executable string) (*Host, error) {
	exe, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, fmt.Errorf("the console's own path: %w", err)
	}
	dir := filepath.Dir(filepath.Dir(exe))
	if _, err := os.Stat(filepath.Join(dir, "compose.yaml")); err != nil {
		return nil, fmt.Errorf("compose.yaml is not in %s, the parent of bin/: run ./setup.sh from the clone of stargaite", dir)
	}
	return New(dir), nil
}
