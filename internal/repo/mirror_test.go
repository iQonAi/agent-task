package repo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/iQonAi/agent-task/internal/gitx"
	"github.com/iQonAi/agent-task/internal/store"
)

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// initOrigin builds a local repo to act as a remote, so tests never touch the
// network or need credentials.
func initOrigin(t *testing.T) (dir, url string) {
	t.Helper()
	dir = t.TempDir()
	mustGit(t, dir, "init", "-b", "main")
	commit(t, dir, "README.md", "hello\n", "initial")
	return dir, "file://" + dir
}

func commit(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	mustGit(t, dir, "add", name)
	mustGit(t, dir, "commit", "-m", msg)
}

func TestSyncClonesMirror(t *testing.T) {
	origin, url := initOrigin(t)
	m := NewManager(t.TempDir())

	path, err := m.Sync(context.Background(), "agent-task", url, "")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if path != m.MirrorPath("agent-task") {
		t.Errorf("path = %q, want %q", path, m.MirrorPath("agent-task"))
	}
	if got := mustGit(t, path, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("mirror is not bare: %q", got)
	}
	if got, want := mustGit(t, path, "rev-parse", UpstreamRef("main")), mustGit(t, origin, "rev-parse", "HEAD"); got != want {
		t.Errorf("mirror main = %s, origin HEAD = %s", got, want)
	}
	// New clones use the split layout: upstream under refs/remotes/origin/.
	if got := mustGit(t, path, "config", "--get-all", "remote.origin.fetch"); got != upstreamRefspec {
		t.Errorf("fetch refspec = %q, want %q", got, upstreamRefspec)
	}
	// The staging directory must not survive a successful clone.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp clone dir left behind: %v", err)
	}
}

// The second call must fetch into the existing cache, not re-clone, and must
// pick up commits pushed to the origin in between.
func TestSyncFetchesOnSecondCall(t *testing.T) {
	origin, url := initOrigin(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()

	if _, err := m.Sync(ctx, "agent-task", url, ""); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	commit(t, origin, "second.txt", "x\n", "second")

	path, err := m.Sync(ctx, "agent-task", url, "")
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got, want := mustGit(t, path, "rev-parse", UpstreamRef("main")), mustGit(t, origin, "rev-parse", "HEAD"); got != want {
		t.Errorf("mirror not updated: %s != %s", got, want)
	}
}

// Auth is optional: a token_ref that resolves to nothing must still clone.
func TestSyncWithoutCredential(t *testing.T) {
	_, url := initOrigin(t)
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	if _, err := NewManager(t.TempDir()).Sync(context.Background(), "agent-task", url, "gh-token-agent-task"); err != nil {
		t.Fatalf("sync without credential: %v", err)
	}
}

// Two tasks overlap on one repo: each Sync must keep the other task's branch
// (issue #71). Before the fix, fetch --prune deleted it.
func TestOverlappingTasksKeepBranches(t *testing.T) {
	origin, url := initOrigin(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()

	mirror, err := m.Sync(ctx, "agent-task", url, "")
	if err != nil {
		t.Fatalf("sync a: %v", err)
	}
	branchA := BranchName("claude", "task a", "aaaaaaa")
	wtA, err := m.AddWorktree(ctx, mirror, "task-a", branchA, "main")
	if err != nil {
		t.Fatalf("add a: %v", err)
	}

	// Task B starts while A runs, after upstream moved.
	commit(t, origin, "second.txt", "x\n", "second")
	if _, err := m.Sync(ctx, "agent-task", url, ""); err != nil {
		t.Fatalf("sync b: %v", err)
	}
	branchB := BranchName("claude", "task b", "bbbbbbb")
	wtB, err := m.AddWorktree(ctx, mirror, "task-b", branchB, "main")
	if err != nil {
		t.Fatalf("add b: %v", err)
	}
	if got, want := mustGit(t, wtB.Path, "rev-parse", "HEAD"), mustGit(t, origin, "rev-parse", "HEAD"); got != want {
		t.Errorf("task b base = %s, want upstream tip %s", got, want)
	}

	// A third sync (e.g. a later task) must keep both branches.
	if _, err := m.Sync(ctx, "agent-task", url, ""); err != nil {
		t.Fatalf("sync c: %v", err)
	}
	for _, wt := range []*Worktree{wtA, wtB} {
		mustGit(t, mirror, "rev-parse", "--verify", "refs/heads/"+wt.Branch)
		if got := mustGit(t, wt.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != wt.Branch {
			t.Errorf("worktree HEAD = %q, want %q", got, wt.Branch)
		}
	}

	// Cleanup still deletes the task branch.
	if err := m.RemoveWorktree(ctx, mirror, "task-a", branchA); err != nil {
		t.Fatalf("remove a: %v", err)
	}
	if _, err := gitx.Run(ctx, mirror, "rev-parse", "--verify", "refs/heads/"+branchA); err == nil {
		t.Errorf("branch %s survived RemoveWorktree", branchA)
	}
}

// A mirror made by the old `clone --mirror` code must be migrated in place on
// the next Sync, without losing a task branch that already exists in it.
func TestSyncMigratesOldMirror(t *testing.T) {
	origin, url := initOrigin(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()

	// Build the old layout by hand, with a task branch made by the old code.
	mirror := m.MirrorPath("agent-task")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustGit(t, filepath.Dir(mirror), "clone", "--mirror", "--quiet", url, mirror)
	oldBranch := BranchName("claude", "old task", "0000000")
	oldWt := m.WorktreePath("task-old")
	mustGit(t, mirror, "worktree", "add", "-b", oldBranch, oldWt, "refs/heads/main")

	commit(t, origin, "second.txt", "x\n", "second")
	for i := 0; i < 2; i++ { // the second Sync checks idempotence
		if _, err := m.Sync(ctx, "agent-task", url, ""); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
		if got := mustGit(t, mirror, "config", "--get-all", "remote.origin.fetch"); got != upstreamRefspec {
			t.Errorf("sync %d: fetch refspec = %q, want %q", i, got, upstreamRefspec)
		}
		if got := mustGit(t, mirror, "config", "--get", "remote.origin.mirror"); got != "false" {
			t.Errorf("sync %d: remote.origin.mirror = %q, want false", i, got)
		}
		mustGit(t, oldWt, "rev-parse", "HEAD")
	}
	if got, want := mustGit(t, mirror, "rev-parse", UpstreamRef("main")), mustGit(t, origin, "rev-parse", "HEAD"); got != want {
		t.Errorf("upstream main = %s, want %s", got, want)
	}

	// A new task on the migrated mirror, then another sync: both branches stay.
	newBranch := BranchName("claude", "new task", "1111111")
	if _, err := m.AddWorktree(ctx, mirror, "task-new", newBranch, "main"); err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	if _, err := m.Sync(ctx, "agent-task", url, ""); err != nil {
		t.Fatalf("sync after add: %v", err)
	}
	for _, b := range []string{oldBranch, newBranch} {
		mustGit(t, mirror, "rev-parse", "--verify", "refs/heads/"+b)
	}
}

func TestCloneURL(t *testing.T) {
	got := CloneURL(store.Repo{Owner: "iQonAi", Repo: "agent-task"})
	if want := "https://github.com/iQonAi/agent-task.git"; got != want {
		t.Errorf("CloneURL = %q, want %q", got, want)
	}
}
