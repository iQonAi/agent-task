package repo

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestExportAndApplyRoundTrip mirrors the M3 transfer: mirror → worktree at base
// → standalone export → (container makes a commit + bundle) → apply onto the
// feature branch.
func TestExportAndApplyRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, url := initOrigin(t)
	m := NewManager(t.TempDir())
	mirror, err := m.Sync(ctx, "agent-task", url, "")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	branch := BranchName("claude", "add feature", "abc1234")
	wt, err := m.AddWorktree(ctx, mirror, "task-1", branch, "main")
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	base := mustGit(t, wt.Path, "rev-parse", "HEAD")

	// Export a standalone copy at base.
	exportDir := filepath.Join(t.TempDir(), "export")
	if err := m.BuildExport(ctx, mirror, branch, "main", exportDir); err != nil {
		t.Fatalf("build export: %v", err)
	}
	if remotes := mustGit(t, exportDir, "remote"); remotes != "" {
		t.Errorf("export is not standalone, has remotes: %q", remotes)
	}
	exportBase, err := m.ExportBase(ctx, exportDir)
	if err != nil {
		t.Fatalf("export base: %v", err)
	}
	if exportBase != base {
		t.Errorf("export base %s != worktree base %s", exportBase, base)
	}

	// Simulate the container: a commit on top of base, bundled base..HEAD.
	commit(t, exportDir, "agent.txt", "the agent's change\n", "agent commit")
	bundle := filepath.Join(t.TempDir(), "changes.bundle")
	mustGit(t, exportDir, "bundle", "create", bundle, exportBase+"..HEAD")

	// Apply onto the feature branch.
	res, err := m.ApplyBundle(ctx, wt.Path, bundle)
	if err != nil {
		t.Fatalf("apply bundle: %v", err)
	}
	if res.Commits != 1 {
		t.Errorf("applied %d commits, want 1", res.Commits)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "agent.txt")); err != nil {
		t.Errorf("agent change not present in worktree: %v", err)
	}
	if b := mustGit(t, wt.Path, "rev-parse", "--abbrev-ref", "HEAD"); b != branch {
		t.Errorf("worktree left branch %q, want %q", b, branch)
	}
	if head := mustGit(t, wt.Path, "rev-parse", "HEAD"); head != res.Head {
		t.Errorf("ApplyResult.Head %s != worktree HEAD %s", res.Head, head)
	}
}

// Another task's Sync can move the upstream ref between AddWorktree and
// BuildExport. The export must still start at the worktree base, so the
// applied commit count holds only the agent's commits.
func TestExportBaseFollowsWorktreeWhenUpstreamMoves(t *testing.T) {
	ctx := context.Background()
	origin, url := initOrigin(t)
	m := NewManager(t.TempDir())
	mirror, err := m.Sync(ctx, "agent-task", url, "")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	branch := BranchName("claude", "add feature", "abc1234")
	wt, err := m.AddWorktree(ctx, mirror, "task-1", branch, "main")
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	base := mustGit(t, wt.Path, "rev-parse", "HEAD")

	// An overlapping task syncs after upstream moved.
	commit(t, origin, "upstream.txt", "moved\n", "upstream moves")
	if _, err := m.Sync(ctx, "agent-task", url, ""); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if moved := mustGit(t, mirror, "rev-parse", UpstreamRef("main")); moved == base {
		t.Fatalf("upstream ref did not move")
	}

	exportDir := filepath.Join(t.TempDir(), "export")
	if err := m.BuildExport(ctx, mirror, branch, "main", exportDir); err != nil {
		t.Fatalf("build export: %v", err)
	}
	exportBase, err := m.ExportBase(ctx, exportDir)
	if err != nil {
		t.Fatalf("export base: %v", err)
	}
	if exportBase != base {
		t.Errorf("export base %s != worktree base %s", exportBase, base)
	}
	if b := mustGit(t, exportDir, "rev-parse", "--abbrev-ref", "HEAD"); b != "main" {
		t.Errorf("export branch = %q, want main", b)
	}

	commit(t, exportDir, "agent.txt", "the agent's change\n", "agent commit")
	bundle := filepath.Join(t.TempDir(), "changes.bundle")
	mustGit(t, exportDir, "bundle", "create", bundle, exportBase+"..HEAD")
	res, err := m.ApplyBundle(ctx, wt.Path, bundle)
	if err != nil {
		t.Fatalf("apply bundle: %v", err)
	}
	if res.Commits != 1 {
		t.Errorf("applied %d commits, want 1", res.Commits)
	}
}

// A non-fast-forward bundle (commits that do not descend from base) must be
// rejected, never force-applied.
func TestApplyBundleRejectsNonFastForward(t *testing.T) {
	ctx := context.Background()
	_, url := initOrigin(t)
	m := NewManager(t.TempDir())
	mirror, err := m.Sync(ctx, "agent-task", url, "")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	branch := BranchName("claude", "x", "abc1234")
	wt, err := m.AddWorktree(ctx, mirror, "task-1", branch, "main")
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	// Export at the ORIGINAL base. The export copies the task branch, so it
	// must be built before the branch advances.
	exportDir := filepath.Join(t.TempDir(), "export")
	if err := m.BuildExport(ctx, mirror, branch, "main", exportDir); err != nil {
		t.Fatalf("export: %v", err)
	}
	// Advance the feature branch so the bundle's base no longer matches HEAD.
	commit(t, wt.Path, "host.txt", "host advanced\n", "host commit")

	// Build a divergent bundle from the export.
	exportBase, _ := m.ExportBase(ctx, exportDir)
	commit(t, exportDir, "agent.txt", "divergent\n", "agent commit")
	bundle := filepath.Join(t.TempDir(), "changes.bundle")
	mustGit(t, exportDir, "bundle", "create", bundle, exportBase+"..HEAD")

	if _, err := m.ApplyBundle(ctx, wt.Path, bundle); err == nil {
		t.Fatal("expected non-fast-forward apply to be rejected, got nil")
	}
}
