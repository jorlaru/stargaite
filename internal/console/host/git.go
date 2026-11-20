package host

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// fetchBound bounds git fetch (K19).
	fetchBound = 20 * time.Second
	// mergeBound bounds git merge --ff-only, which runs to its end.
	mergeBound = 2 * time.Minute
)

// ErrMergeStopped is wrapped by Merge's error when the merge did not finish
// within its bound and was stopped: files it wrote may be left untracked.
var ErrMergeStopped = errors.New("git merge did not finish within its bound")

// gitEnv makes a git call fail rather than ask for a password (K19).
func gitEnv() []string {
	return []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes"}
}

// git runs git in the clone within bound, with gitEnv, and returns its
// standard output, its exit code and Capture's error.
func (h *Host) git(ctx context.Context, bound time.Duration, args ...string) (string, int, error) {
	bounded, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	out, code, err := h.capture(bounded, gitEnv(), "git", args...)
	return string(out), code, err
}

// gitOK runs git within h.bound and returns its output trimmed, ok when
// it exited 0.
func (h *Host) gitOK(ctx context.Context, args ...string) (string, bool) {
	out, code, err := h.git(ctx, h.bound, args...)
	return strings.TrimSpace(out), err == nil && code == 0
}

// CloneVersion is the clone's git describe, as setup.sh's clone_version
// takes it, or dev when git is missing or fails.
func (h *Host) CloneVersion(ctx context.Context) string {
	v, ok := h.gitOK(ctx, "describe", "--tags", "--always", "--dirty")
	if !ok || v == "" {
		return "dev"
	}
	return v
}

// Fetch fetches the clone's upstream, as setup.sh's fetch does: after the
// root rule, as git writes to the clone, it refuses what is not a clone, a
// detached HEAD and a branch with no upstream, then fetches within
// fetchBound. It returns the upstream and how many of its commits HEAD
// lacks. A refusal changes nothing.
func (h *Host) Fetch(ctx context.Context) (upstream string, ahead int, err error) {
	if err := h.RootRule(h.UID); err != nil {
		return "", 0, err
	}
	if _, ok := h.gitOK(ctx, "rev-parse", "--git-dir"); !ok {
		return "", 0, errors.New("upgrades come from a git clone of stargaite, with git: see https://git-scm.com/downloads")
	}
	if _, ok := h.gitOK(ctx, "symbolic-ref", "-q", "HEAD"); !ok {
		return "", 0, errors.New("HEAD is detached, so no branch says what to upgrade to: git switch <branch>, then run this again")
	}
	if _, ok := h.gitOK(ctx, "rev-parse", "--abbrev-ref", "@{upstream}"); !ok {
		branch, _ := h.gitOK(ctx, "branch", "--show-current")
		return "", 0, fmt.Errorf("branch %s has no upstream to upgrade from: git branch --set-upstream-to <remote>/<branch>", branch)
	}
	_, code, err := h.git(ctx, h.fetchBound, "fetch", "--quiet")
	switch {
	case ctx.Err() != nil:
		return "", 0, fmt.Errorf("git fetch: %w", ctx.Err())
	case err != nil || code != 0:
		return "", 0, errors.New("git fetch failed or did not finish within 20 s; nothing was changed")
	}
	upstream, ahead, ok := h.Ahead(ctx)
	if !ok {
		return "", 0, errors.New("git rev-list failed; nothing was changed")
	}
	return upstream, ahead, nil
}

// Ahead is the clone's upstream and how many of its commits HEAD lacks, from
// the remote-tracking ref alone: it never fetches. Any failure is ok false.
func (h *Host) Ahead(ctx context.Context) (upstream string, ahead int, ok bool) {
	upstream, ok = h.gitOK(ctx, "rev-parse", "--abbrev-ref", "@{upstream}")
	if !ok {
		return "", 0, false
	}
	n, ok := h.gitOK(ctx, "rev-list", "--count", "HEAD..@{upstream}")
	ahead, err := strconv.Atoi(n)
	return upstream, ahead, ok && err == nil
}

// Clean reports whether the clone has no local change to a tracked file. A
// git that is missing or fails counts as clean, as setup.sh's check did, so
// Fetch's refusal is the one said.
func (h *Host) Clean(ctx context.Context) bool {
	out, ok := h.gitOK(ctx, "status", "--porcelain", "--untracked-files=no")
	return !ok || out == ""
}

// FastForwards reports whether HEAD is an ancestor of the upstream.
func (h *Host) FastForwards(ctx context.Context) (bool, error) {
	return h.gitTest(ctx, 0, "merge-base", "--is-ancestor", "HEAD", "@{upstream}")
}

// MigrationsChanged reports whether the upstream changes the store's
// migrations.
func (h *Host) MigrationsChanged(ctx context.Context) (bool, error) {
	return h.gitTest(ctx, 1, "diff", "--quiet", "HEAD", "@{upstream}", "--", "internal/store/migrations")
}

// gitTest runs a git that answers yes or no by its exit, 0 or 1: yes is the
// exit given. Another exit is an error.
func (h *Host) gitTest(ctx context.Context, yes int, args ...string) (bool, error) {
	_, code, err := h.git(ctx, h.bound, args...)
	switch {
	case err != nil:
		return false, err
	case code > 1:
		return false, fmt.Errorf("git %s failed with exit %d", args[0], code)
	}
	return code == yes, nil
}

// NewCommits is the upstream's commits HEAD lacks, the newest 20, one line
// each.
func (h *Host) NewCommits(ctx context.Context) ([]string, error) {
	out, ok := h.gitOK(ctx, "--no-pager", "log", "--oneline", "-20", "HEAD..@{upstream}")
	if !ok {
		return nil, errors.New("git log failed")
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// Commit is the full id of the commit rev names, or "" when it names none.
// A rev that would read as a flag names none.
func (h *Host) Commit(ctx context.Context, rev string) string {
	if strings.HasPrefix(rev, "-") {
		return ""
	}
	id, ok := h.gitOK(ctx, "rev-parse", "-q", "--verify", rev+"^{commit}")
	if !ok {
		return ""
	}
	return id
}

// Merge fast-forwards the clone to its upstream within mergeBound, git's
// lines kept back and its last ones said when it fails. At the bound git is
// stopped and the error wraps ErrMergeStopped. The caller passes a context
// that the reader cannot end: a merge stopped halfway leaves files behind.
func (h *Host) Merge(ctx context.Context, say func(string)) error {
	var tail kept
	over, failed, err := h.long(ctx, h.mergeBound, gitEnv(), tail.add, "git", "merge", "--ff-only", "--quiet", "@{upstream}")
	switch {
	case err != nil:
		return fmt.Errorf("git merge: %w", err)
	case over:
		return fmt.Errorf("git merge --ff-only: %w", ErrMergeStopped)
	case failed:
		tail.say(say, "git merge's last lines:")
		return errors.New("git merge --ff-only failed")
	}
	return nil
}
