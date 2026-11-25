// Package repo manages host-side git state for registred repositories: the
// bare mirror cache and, later, feature-branch worktrees
package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/iQonAi/agent-task/internal/gitx"
	"github.com/iQonAi/agent-task/internal/store"
)

// tokenEnvVar carries the credential to git's helper thorugh the environment,
// so it never appears in argv (/proc/<pid>/cmdline is world-readable).
const tokenEnvVar = "AGENT_TASK_GIT_TOKEN"

// credentialHelper is a one-shot shell function git runs when a remote asks for
// authentication. Github accepts any username when the password is a PAT.
const credentialHelper = `!f() { echo username=x-access-token; echo password=$` + tokenEnvVar + `; }; f`

// upstreamRefspec keeps upstream branches under refs/remotes/origin/, apart from
// the task branches that AddWorktree creates under refs/heads/. A fetch --prune
// then only removes refs that came from upstream, never a running task's
// branch (issue #71).
const upstreamRefspec = "+refs/heads/*:refs/remotes/origin/*"

// UpstreamRef is the mirror's copy of an upstream branch.
func UpstreamRef(branch string) string {
	return "refs/remotes/origin/" + branch
}

// Manager owns the host-side layout under data_dir
type Manager struct {
	dataDir string
}

func NewManager(dataDir string) *Manager {
	return &Manager{dataDir: dataDir}
}

// CloneURL is the HTTPS remote a registerd repo.
func CloneURL(r store.Repo) string {
	return fmt.Sprintf("https://github.com/%s/%s.git", r.Owner, r.Repo)
}

// MirrorPath is where a repo's bare mirror lives. Deterministic, so a restart
// finds the existing cahce instead of re-cloning.
func (m *Manager) MirrorPath(name string) string {
	return filepath.Join(m.dataDir, "mirrors", name+".git")
}

// Sync ensures a bare mirror of url exists for name and is current, returning
// its path. Safe to call repeatedly: the first call clones, later ones fetch.
// token is the raw credential (empty = anonymous); the caller resolves it, so
// repo has no dependency on how credentials are stored (D3 keeps the token out
// of the container regardless).
func (m *Manager) Sync(ctx context.Context, name, url, token string) (string, error) {
	path := m.MirrorPath(name)

	authArgs, env := auth(token)

	// HEAD, not the directory: a half-finished clone leaves a directory behind.
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat mirror %s: %w", path, err)
		}
		if err := m.clone(ctx, path, url, authArgs, env); err != nil {
			return "", err
		}
		return path, nil
	}

	// Rewrite an old-layout mirror before it fetches: its +refs/*:refs/*
	// refspec would prune the task branches.
	if err := migrate(ctx, path); err != nil {
		return "", fmt.Errorf("migrate mirror %s: %w", name, err)
	}

	// --prune drops refs who upstream branch was deleted, so the cache does
	// not acccumulate every branch the repo has ever had. It only touches
	// refs/remotes/origin/, so task branches survive.
	if _, err := gitx.RunEnv(ctx, path, env, gitArgs(authArgs, "fetch", "--prune", "--quiet")...); err != nil {
		return "", fmt.Errorf("fetch mirror %s: %w", name, err)
	}
	return path, nil
}

func (m *Manager) clone(ctx context.Context, path, url string, authArgs, env []string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create mirror dir: %w", err)
	}

	// Clone to a temp path and rename on success, so an interrupted clone can
	// never leave a partial directory that later looks like a valid cache.
	tmp := path + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clear partial mirror: %w", err)
	}

	// A bare repo with a normal origin remote, not clone --mirror: remote add
	// sets upstreamRefspec, so upstream branches land in refs/remotes/origin/
	// and refs/heads/ holds only task branches.
	if _, err := gitx.Run(ctx, parent, "init", "--bare", "--quiet", tmp); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("init mirror: %w", err)
	}
	if _, err := gitx.Run(ctx, tmp, "remote", "add", "origin", url); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("add mirror remote: %w", err)
	}
	if _, err := gitx.RunEnv(ctx, tmp, env, gitArgs(authArgs, "fetch", "--quiet", "origin")...); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("clone mirror: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("publish mirror: %w", err)
	}
	return nil
}

// migrate moves a mirror made by `clone --mirror` to the upstreamRefspec
// layout. It only rewrites config, so it never deletes a ref; the old upstream
// copies in refs/heads/ stay behind, unused. Idempotent: each step is safe to
// repeat, and the refspec is written last, so an interrupted run is redone.
func migrate(ctx context.Context, path string) error {
	fetch, err := gitx.Run(ctx, path, "config", "--get-all", "remote.origin.fetch")
	if err != nil {
		return fmt.Errorf("read refspec: %w", err)
	}
	if fetch == upstreamRefspec {
		return nil
	}
	// mirror=true makes a plain `git push origin` push every ref.
	if _, err := gitx.Run(ctx, path, "config", "remote.origin.mirror", "false"); err != nil {
		return fmt.Errorf("clear mirror flag: %w", err)
	}
	// clone --mirror sets tagOpt=--no-tags; a new mirror follows tags.
	// Exit status 5 means the key is already absent.
	if _, err := gitx.Run(ctx, path, "config", "--unset-all", "remote.origin.tagOpt"); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 5 {
			return fmt.Errorf("clear tagOpt: %w", err)
		}
	}
	if _, err := gitx.Run(ctx, path, "config", "--replace-all", "remote.origin.fetch", upstreamRefspec); err != nil {
		return fmt.Errorf("set refspec: %w", err)
	}
	return nil
}

// gitArgs joins the -c auth flags with a subcommand. It copies rather than
// appending in place: append can write into the caller's backing array when
// capacity allows, so two calls sharing one authArgs would corrupt eachother.
func gitArgs(authArgs []string, sub ...string) []string {
	out := make([]string, 0, len(authArgs)+len(sub))
	out = append(out, authArgs...)
	return append(out, sub...)
}

// auth returns the git -c arguments and environment for authenticating as the
// machine user, or nils for an empty token — a public repo clones anonymously.
// The token travels in the environment (never argv) and is consumed by a
// one-shot credential helper.
func auth(token string) (args []string, env []string) {
	if token == "" {
		return nil, nil
	}
	// The empty value clears any inherited helper before ours is appended.
	return []string{"-c", "credential.helper=", "-c", "credential.helper=" + credentialHelper},
		[]string{tokenEnvVar + "=" + token}
}
