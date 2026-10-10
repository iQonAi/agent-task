// Package github performs host-side GitHub operations — issue fetch, feature-
// branch push, PR create, issue comment — using a repo-scoped machine-user
// token (D3). The token lives ONLY here and only on the host: it is passed to
// gh via the environment (never argv) and to git via a one-shot credential
// helper, and it is never placed in a container, the source export, the bundle,
// or the DB. A compromised agent has no token to steal.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/iQonAi/agent-task/internal/gitx"
	"github.com/iQonAi/agent-task/internal/prompt"
)

// tokenEnvVar carries the token to git's credential helper via the environment.
const tokenEnvVar = "AGENT_TASK_GH_TOKEN"

const credentialHelper = `!f() { echo username=x-access-token; echo password=$` + tokenEnvVar + `; }; f`

// Client runs GitHub operations for one repo with one token.
type Client struct {
	owner string
	repo  string
	token string
}

// New returns a client for owner/repo authenticating with token.
func New(owner, repo, token string) *Client {
	return &Client{owner: owner, repo: repo, token: token}
}

func (c *Client) slug() string { return c.owner + "/" + c.repo }

// envWithout returns os.Environ() with the named keys removed, so a caller can
// append its own single authoritative value.
func envWithout(keys ...string) []string {
	var out []string
	for _, e := range os.Environ() {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(e, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
}

// gh runs the gh CLI with the token in GH_TOKEN (not argv) and returns stdout.
// dir is the working directory ("" = inherit); some gh subcommands (pr create)
// shell out to git and must run inside a git repo.
func (c *Client) gh(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Drop any inherited GH_TOKEN/GH_PROMPT_DISABLED before adding ours: a
	// duplicate key would let getenv return the inherited value first, so gh
	// could authenticate with the operator's token instead of the repo-scoped
	// one — breaking the D3 boundary.
	cmd.Env = append(envWithout("GH_TOKEN", "GH_PROMPT_DISABLED"),
		"GH_TOKEN="+c.token, "GH_PROMPT_DISABLED=1")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		// args carry no secret (the token is in the env), so they are safe to echo.
		return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// FetchIssue renders an issue into the prompt input (D8 --issue).
func (c *Client) FetchIssue(ctx context.Context, number int) (prompt.Issue, error) {
	out, err := c.gh(ctx, "", "issue", "view", strconv.Itoa(number),
		"--repo", c.slug(), "--json", "number,title,body,url")
	if err != nil {
		return prompt.Issue{}, err
	}
	return parseIssue(out)
}

// parseIssue decodes the gh issue-view JSON. Split out so it is testable
// without invoking gh.
func parseIssue(jsonStr string) (prompt.Issue, error) {
	var raw struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return prompt.Issue{}, fmt.Errorf("parse issue json: %w", err)
	}
	return prompt.Issue{Number: raw.Number, Title: raw.Title, Body: raw.Body, URL: raw.URL}, nil
}

// Push pushes the feature branch using the token via a credential helper (never
// in argv or written to .git/config). It pushes to the explicit repo URL rather
// than the `origin` remote: the mirror clone sets remote.origin.mirror=true,
// which rejects a single-branch refspec ("--mirror can't be combined with
// refspecs"). All git runs hook-disabled (gitx).
func (c *Client) Push(ctx context.Context, worktreeDir, branch string) error {
	url := fmt.Sprintf("https://github.com/%s/%s.git", c.owner, c.repo)
	env := []string{tokenEnvVar + "=" + c.token}
	args := []string{
		"-c", "credential.helper=",
		"-c", "credential.helper=" + credentialHelper,
		"push", url, branch,
	}
	if _, err := gitx.RunEnv(ctx, worktreeDir, env, args...); err != nil {
		return fmt.Errorf("push %s: %w", branch, err)
	}
	return nil
}

// OpenPR opens a pull request (base = the default branch) and returns its URL.
// Runs from worktreeDir because `gh pr create` shells out to git and needs a
// git repo. Never auto-merged.
func (c *Client) OpenPR(ctx context.Context, worktreeDir, branch, base, title, body string) (string, error) {
	bodyFile, err := os.CreateTemp("", "pr-body-")
	if err != nil {
		return "", fmt.Errorf("pr body file: %w", err)
	}
	defer os.Remove(bodyFile.Name())
	if _, err := bodyFile.WriteString(body); err != nil {
		bodyFile.Close()
		return "", fmt.Errorf("write pr body: %w", err)
	}
	if err := bodyFile.Close(); err != nil {
		return "", fmt.Errorf("close pr body: %w", err)
	}

	out, err := c.gh(ctx, worktreeDir, "pr", "create", "--repo", c.slug(),
		"--base", base, "--head", branch, "--title", title, "--body-file", bodyFile.Name())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil // gh prints the PR URL
}

// CommentIssue posts a comment (used to back-link the PR onto the source issue).
func (c *Client) CommentIssue(ctx context.Context, number int, body string) error {
	_, err := c.gh(ctx, "", "issue", "comment", strconv.Itoa(number), "--repo", c.slug(), "--body", body)
	return err
}

// CheckAccess verifies the client's token can see owner/repo (used by
// `repos doctor`). It returns gh's raw error on failure. GitHub returns the
// same 404 whether the repo genuinely doesn't exist/was renamed or a
// fine-grained token simply hasn't been approved by the org owner yet
// (docs/runbook/0001-agent-task-vm.md's "Gotcha" note) -- the two are not
// distinguishable from this response alone, so this method does not attempt
// to tell them apart; that is left to the caller's message.
func (c *Client) CheckAccess(ctx context.Context) error {
	_, err := c.gh(ctx, "", "api", "repos/"+c.slug())
	return err
}
