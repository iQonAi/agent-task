package repo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/iQonAi/agent-task/internal/gitx"
)

// BuildExport creates a self-contained copy of the task branch taskBranch in
// srcRepo into destDir, checked out under the name branch (the default
// branch): a standalone git repo with its own object store and NO reference
// back to srcRepo. This is the copy handed to the container (§8.4). It copies
// the task branch, not the upstream ref, because AddWorktree fixed the task
// branch at the base: another task's Sync can move the upstream ref, but the
// export base still equals the worktree base. clone --branch cannot rename the
// branch, so the export is built with init + fetch. A fetch from a local path
// sends a pack, so the container shares no inodes with the host mirror. Only
// the one branch (and tags in its history) is fetched, and no remote is
// configured, so there is no URL for a hostile agent to fetch from or push to.
func (m *Manager) BuildExport(ctx context.Context, srcRepo, taskBranch, branch, destDir string) error {
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return fmt.Errorf("create export parent: %w", err)
	}
	if _, err := gitx.Run(ctx, filepath.Dir(destDir), "init", "--quiet", "-b", branch, destDir); err != nil {
		return fmt.Errorf("init export: %w", err)
	}
	// --update-head-ok: HEAD already names the (unborn) branch being fetched.
	if _, err := gitx.Run(ctx, destDir,
		"fetch", "--quiet", "--update-head-ok", srcRepo, "refs/heads/"+taskBranch+":refs/heads/"+branch,
	); err != nil {
		return fmt.Errorf("build export of %s@%s: %w", srcRepo, branch, err)
	}
	if _, err := gitx.Run(ctx, destDir, "checkout", "--quiet", "--force", branch); err != nil {
		return fmt.Errorf("check out export: %w", err)
	}
	return nil
}

// ExportBase returns the commit the export/worktree started from — the tip of
// branch in the export. The container bundles base..HEAD, and the host applies
// that onto the feature branch.
func (m *Manager) ExportBase(ctx context.Context, dir string) (string, error) {
	rev, err := gitx.Run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read export base: %w", err)
	}
	return rev, nil
}
