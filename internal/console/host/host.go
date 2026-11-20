// Package host is the console's access to the machine it runs on: the clone
// it was exported into, and the commands it runs there, docker compose and
// git. It is the console's only package that runs a command, and every
// command it runs is bounded and stopped with its whole process group when
// the console stops (OPS-11: every wait is bounded).
package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// localBound bounds each local read: docker compose ps and git describe.
	localBound = 20 * time.Second
	// upBound bounds docker compose up, which may pull and build.
	upBound = 20 * time.Minute
	// stopBound bounds docker compose stop, restart and down: the container
	// has 60 s to stop (stop_grace_period in compose.yaml).
	stopBound = 2 * time.Minute
	// followBound bounds a follow of the logs (OPS-11).
	followBound = time.Hour
)

// Host is the clone. Every command runs with its directory as the working
// directory. Its bounds are fields that tests shorten, never the constants.
// UID is the user the console runs as: every root decision reads it.
type Host struct {
	Dir         string
	UID         int
	bound       time.Duration // localBound
	fetchBound  time.Duration // fetchBound
	mergeBound  time.Duration // mergeBound
	upBound     time.Duration // upBound
	stopBound   time.Duration // stopBound
	followBound time.Duration // followBound
	httpBound   time.Duration // httpBound
	ready       struct {      // readiness: at most tries checks, pause apart, each try long, all within bound
		tries             int
		pause, try, bound time.Duration
	}
	readyCheck func(context.Context) error // one readiness check: Ready; tests replace it
}

// New returns the clone at dir.
func New(dir string) *Host {
	h := &Host{Dir: dir, UID: os.Getuid(), bound: localBound, fetchBound: fetchBound, mergeBound: mergeBound, upBound: upBound,
		stopBound: stopBound, followBound: followBound, httpBound: httpBound}
	h.ready.tries, h.ready.pause, h.ready.try, h.ready.bound = 120, time.Second, 2*time.Second, 120*time.Second
	h.readyCheck = h.Ready
	return h
}

// ShortenFollow sets the bound of a follow of the logs to d: the tasks'
// tests use it, since the bounds are fields no other package sets.
func (h *Host) ShortenFollow(d time.Duration) { h.followBound = d }

// ShortenWaits sets the long waits to d, the start, the merge and
// readiness, and readiness to three tries a millisecond apart, for the
// tasks' tests.
func (h *Host) ShortenWaits(d time.Duration) {
	h.upBound, h.mergeBound = d, d
	h.ready.tries, h.ready.pause, h.ready.bound = 3, time.Millisecond, d
}

// Find returns the clone the console was exported into: the parent of the
// executable's directory, bin/, which must hold compose.yaml, run by uid. It
// applies the root rule for uid, then changes to the clone, so a Host from
// Find has passed both.
func Find(executable string, uid int) (*Host, error) {
	exe, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, fmt.Errorf("the console's own path: %w", err)
	}
	dir := filepath.Dir(filepath.Dir(exe))
	if _, err := os.Stat(filepath.Join(dir, "compose.yaml")); err != nil {
		return nil, fmt.Errorf("compose.yaml is not in %s, the parent of bin/: run ./setup.sh from the clone of stargaite", dir)
	}
	h := New(dir)
	h.UID = uid
	if err := h.RootRule(uid); err != nil {
		return nil, err
	}
	if err := os.Chdir(dir); err != nil {
		return nil, fmt.Errorf("the clone: %w", err)
	}
	return h, nil
}

// RootRule refuses root in a clone that belongs to another user, before
// anything is written, as setup.sh's root_rule does.
func (h *Host) RootRule(uid int) error {
	info, err := os.Stat(h.Dir)
	if err != nil {
		return fmt.Errorf("the clone: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("the clone's owner is not known on this system")
	}
	return rootRule(uid, int(st.Uid))
}

func rootRule(uid, owner int) error {
	if uid == 0 && owner != 0 {
		return errors.New("run as root in a clone that belongs to another user, the console would leave root-owned files " +
			"(.env, tls/, backups/, git objects) that break its next run and that user's git: run it as the clone's owner")
	}
	return nil
}
