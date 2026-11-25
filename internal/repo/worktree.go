package repo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/iQonAi/agent-task/internal/gitx"
)

// Worktree is a checked-out feature branch on the host, one per task.
type Worktree struct {
	Path   string
	Branch string
}

// WorktreePath is where a task's worktree lives. Keyed by task id, so it is
// unique and a restart can find it again
func (m *Manager) WorktreePath(taskID string) string {
	return filepath.Join(m.dataDir, "worktrees", taskID)
}

// AddWorktree creates a worktree at a fresh branch off the mirror's upstream
// copy of the default branch. mirrorPath must be an existing mirror (from Sync).
func (m *Manager) AddWorktree(ctx context.Context, mirrorPath, taskID, branch, defaultBranch string) (*Worktree, error) {
	path := m.WorktreePath(taskID)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create worktree dir: %w", err)
	}

	// git worktree add -b <branch> <path> <start-point>. Run from inside the
	// mirror so git knows which repository owns the worktree. The start point
	// is the mirror's upstream copy of the default branch, not a remote lookup.
	// --no-track: the task branch has no upstream to follow.
	if _, err := gitx.Run(
		ctx, mirrorPath,
		"worktree", "add", "--no-track", "-b", branch, path, UpstreamRef(defaultBranch),
	); err != nil {
		return nil, fmt.Errorf("add worktree for %s : %w", taskID, err)
	}

	return &Worktree{Path: path, Branch: branch}, nil
}

// RemoveWorktree deletes a task's worktree and it's branch. Idempotent: a
// missing worktree or branch is not an error, so cleanup and the startup sweep
// can run without first checking what still exists.
func (m *Manager) RemoveWorktree(ctx context.Context, mirrorPath, taskID, branch string) error {
	path := m.WorktreePath(taskID)

	if _, err := os.Stat(path); err == nil {
		// --force: the agent's checkout is expected to be dirty.
		if _, err := gitx.Run(ctx, mirrorPath, "worktree", "remove", "--force", path); err != nil {
			return fmt.Errorf("remove worktree %s: %w", taskID, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat worktree %s: %w", path, err)
	}

	// Prune administrative records for any worktree whose directory is already
	// gone, so git's metadata does not drift from the filesystem.
	if _, err := gitx.Run(ctx, mirrorPath, "worktree", "prune"); err != nil {
		return fmt.Errorf("prune worktrees: %w", err)
	}

	// Delete the branch last. -D because it is an unmerged feature branch;
	// ignore "not found" so a re-run stays clean
	if branch != "" {
		if _, err := gitx.Run(ctx, mirrorPath, "branch", "-D", branch); err != nil && !isMissingBranch(err) {
			return fmt.Errorf("delete branch %s: %w", branch, err)
		}
	}

	return nil
}

func isMissingBranch(err error) bool {
	return strings.Contains(err.Error(), "not found")
}
