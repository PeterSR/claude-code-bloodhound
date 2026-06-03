// Package gitmeta resolves live git facts about a worktree path —
// current branch, repo display name, and the common git dir that groups
// all worktrees of one repo. Trail stores absolute worktree paths and
// derives these at render time so a branch shown in the UI is current
// truth, not a stale snapshot from when the session was analysed.
//
// Everything degrades gracefully: a path that's gone, isn't a git
// worktree, or where `git` isn't on PATH yields zero values, never an
// error that callers must handle.
package gitmeta

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Info is the live view of a worktree path.
type Info struct {
	// Dirname is filepath.Base(path) — the worktree directory name, the
	// natural display label (e.g. "caterflow-new-k8s-platform").
	Dirname string
	// Branch is the current checked-out branch, or "" if detached /
	// unavailable.
	Branch string
	// Exists is true when the path is a directory we could resolve.
	Exists bool
	// CommonDir is `git rev-parse --git-common-dir` resolved absolute —
	// the same value for every worktree of one repo, so it groups
	// worktrees. "" when not a git worktree.
	CommonDir string
	// Root is `git rev-parse --show-toplevel` — the worktree's top
	// directory. "" when not a git worktree (caller falls back to the
	// path itself).
	Root string
}

// Look resolves live info for a worktree path. Cheap (a couple of short
// git invocations); intended to be called per repo row at render time.
// Branch resolution falls back to branchCached when git can't answer.
func Look(path, branchCached string) Info {
	info := Info{Dirname: filepath.Base(path), Branch: branchCached}
	if path == "" {
		return info
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Branch (current truth); keep branchCached on failure.
	if b, ok := gitOut(ctx, path, "branch", "--show-current"); ok {
		info.Exists = true
		if b != "" {
			info.Branch = b
		}
	}
	// Common dir groups worktrees of the same repo.
	if cd, ok := gitOut(ctx, path, "rev-parse", "--git-common-dir"); ok && cd != "" {
		info.Exists = true
		if !filepath.IsAbs(cd) {
			cd = filepath.Join(path, cd)
		}
		info.CommonDir = filepath.Clean(cd)
	}
	// Worktree top dir — the natural repo_path even when cwd is a subdir.
	if root, ok := gitOut(ctx, path, "rev-parse", "--show-toplevel"); ok && root != "" {
		info.Exists = true
		info.Root = filepath.Clean(root)
		info.Dirname = filepath.Base(info.Root)
	}
	return info
}

func gitOut(ctx context.Context, dir string, args ...string) (string, bool) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
