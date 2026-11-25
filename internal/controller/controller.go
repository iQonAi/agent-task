// Package controller runs a single task end-to-end: render prompt → sync repo →
// create feature worktree → export source → run the agent in an isolated
// container → apply the resulting bundle onto the feature branch → collect
// artifacts. This is the M3 slice; the state machine, worker pool, cancellation,
// and daemon integration are M5.
package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iQonAi/agent-task/internal/agent"
	"github.com/iQonAi/agent-task/internal/github"
	"github.com/iQonAi/agent-task/internal/prompt"
	"github.com/iQonAi/agent-task/internal/repo"
	"github.com/iQonAi/agent-task/internal/runner"
	"github.com/iQonAi/agent-task/internal/store"
)

// Terminal states (D9).
const (
	StateCompleted = "Completed"
	StateFailed    = "Failed"
	StateCancelled = "Cancelled"
)

// Cancel causes: attached via context.WithCancelCause so recording can tell an
// operator cancel (-> Cancelled) from a daemon shutdown (-> Failed, matching
// the recovery wording). Defined here — not in pool — because pool already
// imports controller and both packages need them.
var (
	ErrUserCancel = errors.New("cancelled by user")
	ErrShutdown   = errors.New("interrupted by daemon shutdown")
)

// ContextOutcome maps a done task context to its terminal state and reason
// (§7.4): timeout -> Failed "timeout"; daemon shutdown -> Failed with the
// recovery wording; any other cancel -> Cancelled "cancelled". ok is false
// while the context is still live.
func ContextOutcome(ctx context.Context) (state, reason string, ok bool) {
	switch {
	case ctx.Err() == nil:
		return "", "", false
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return StateFailed, "timeout", true
	case errors.Is(context.Cause(ctx), ErrShutdown):
		return StateFailed, "interrupted by daemon shutdown", true
	default:
		return StateCancelled, "cancelled", true
	}
}

// Recorder persists lifecycle state, audit events, artifacts, and the PR URL.
// *store.Store satisfies it; nil disables recording (the standalone --local path
// records after the run).
type Recorder interface {
	UpdateTaskState(id, state string) error
	InsertEvent(taskID, eventType, message string) error
	InsertArtifact(taskID, kind, path string) error
	SetTaskPRURL(id, url string) error
	SetTaskBranchWorktree(id, branch, worktree string) error
}

// Limits are the per-run resource caps.
type Limits struct {
	CPUs      string
	MemoryMB  int
	PidsLimit int
	Timeout   time.Duration
}

// Deps are the collaborators a run needs.
type Deps struct {
	Repo     *repo.Manager
	Runner   runner.Runner
	Image    string
	Recorder Recorder // optional
}

// Request is one task to run.
type Request struct {
	TaskID        string
	Title         string // used for the feature-branch slug (issue title when --issue)
	RepoName      string
	RepoURL       string
	Owner         string // GitHub owner, for issue fetch / push / PR
	Repo          string // GitHub repo name
	DefaultBranch string
	IssueNumber   int    // > 0 to render the prompt from an issue (D8 --issue)
	GitHubToken   string // host-only repo-scoped token (D3); "" = no GitHub, anon clone
	Prompt        prompt.Input
	Agent         agent.Agent
	AuthMethod    agent.AuthMethod
	AuthValue     string // model token/key value (M3: from flag/env; M5: LoadCredential)
	Limits        Limits
	WorkDir       string // host scratch dir for prompt, export, and out
}

// Artifact is a file a run produced, ready to be indexed by the caller.
type Artifact struct {
	Kind string
	Path string
}

// Outcome is the result of a run.
type Outcome struct {
	State     string
	Error     string // why a Failed task failed ("" when Completed)
	Commits   int
	ExitCode  int
	Branch    string
	Worktree  string
	OutDir    string
	PRURL     string // set when a PR was opened (M4)
	Artifacts []Artifact
}

// artifactKinds maps a produced filename to its artifact kind, in a fixed order
// so collection is deterministic (stable CLI output and insertion order).
var artifactKinds = []struct{ name, kind string }{
	{"changes.bundle", "bundle"},
	{"transcript.json", "transcript"},
	{"diff.patch", "diff"},
	{"run.log", "log"},
	{"summary.txt", "summary"},
}

// Run executes the task. It returns an Outcome (with a terminal State) on a
// completed pipeline; it returns an error only when the pipeline itself could
// not run (a failing agent is a Failed Outcome, not an error).
func Run(ctx context.Context, deps Deps, req Request) (out Outcome, err error) {
	if req.Agent == nil {
		return Outcome{}, fmt.Errorf("no agent specified")
	}

	// Enter Running and record once up-front.
	deps.setState(req.TaskID, store.StateRunning)
	deps.event(req.TaskID, store.EventState, "Created->Running")

	// Record everything terminal exactly once, at whichever return fires — the
	// returned Outcome becomes `out`. Only when the pipeline completed (err ==
	// nil) and a terminal state was set; a pipeline error leaves it to the daemon.
	defer func() {
		if err != nil || out.State == "" {
			return
		}
		deps.recordArtifacts(req.TaskID, out.Artifacts)
		if out.PRURL != "" && deps.Recorder != nil {
			_ = deps.Recorder.SetTaskPRURL(req.TaskID, out.PRURL)
		}
		deps.setState(req.TaskID, out.State)
		msg := "->" + out.State
		if out.Error != "" {
			msg += ": " + out.Error
		}
		deps.event(req.TaskID, store.EventState, msg)
	}()

	// A GitHub client (host-only, D3) is available when a token + owner/repo are
	// set. The controller only calls its methods — it never puts the token into
	// the runner Spec, so the token cannot reach the container.
	var gh *github.Client
	if req.GitHubToken != "" && req.Owner != "" && req.Repo != "" {
		gh = github.New(req.Owner, req.Repo, req.GitHubToken)
	}

	// 0. Resolve the prompt input and an effective title (issue or free-form).
	promptInput := req.Prompt
	title := req.Title
	issueURL := ""
	if req.IssueNumber > 0 {
		if gh == nil {
			return Outcome{}, fmt.Errorf("--issue requires a GitHub token and owner/repo")
		}
		issue, err := gh.FetchIssue(ctx, req.IssueNumber)
		if err != nil {
			return Outcome{}, err
		}
		promptInput = prompt.Input{Issue: &issue}
		issueURL = issue.URL
		if title == "" {
			title = issue.Title
		}
	}

	// 1. Render the prompt to a host file.
	promptText, err := prompt.Render(promptInput)
	if err != nil {
		return Outcome{}, err
	}
	promptPath := filepath.Join(req.WorkDir, "prompt.md")
	if err := os.MkdirAll(req.WorkDir, 0o755); err != nil {
		return Outcome{}, fmt.Errorf("create work dir: %w", err)
	}
	if err := os.WriteFile(promptPath, []byte(promptText), 0o644); err != nil {
		return Outcome{}, fmt.Errorf("write prompt: %w", err)
	}

	// 2. Sync the mirror and create the feature-branch worktree.
	deps.event(req.TaskID, store.EventPhase, "sync repo")
	mirror, err := deps.Repo.Sync(ctx, req.RepoName, req.RepoURL, req.GitHubToken)
	if err != nil {
		return Outcome{}, err
	}
	branch := repo.BranchName(req.Agent.Name(), title, req.TaskID)
	deps.event(req.TaskID, store.EventPhase, "add worktree")
	wt, err := deps.Repo.AddWorktree(ctx, mirror, req.TaskID, branch, req.DefaultBranch)
	if err != nil {
		return Outcome{}, err
	}
	// Persist branch + worktree as soon as they exist, so the orphan sweep can
	// find them even if this run never reaches a terminal Outcome.
	if deps.Recorder != nil {
		_ = deps.Recorder.SetTaskBranchWorktree(req.TaskID, branch, wt.Path)
	}

	// 3. Build the standalone source export handed to the container.
	exportDir := filepath.Join(req.WorkDir, "export")
	deps.event(req.TaskID, store.EventPhase, "build export")
	if err := deps.Repo.BuildExport(ctx, mirror, wt.Branch, req.DefaultBranch, exportDir); err != nil {
		return Outcome{}, err
	}
	base, err := deps.Repo.ExportBase(ctx, exportDir)
	if err != nil {
		return Outcome{}, err
	}

	// 4. Compose the container command and the agent's env.
	envVar, err := req.Agent.EnvVar(req.AuthMethod)
	if err != nil {
		return Outcome{}, err
	}
	agentCmd, err := req.Agent.Command(req.AuthMethod, runner.PromptPath, runner.OutPath+"/transcript.json")
	if err != nil {
		return Outcome{}, err
	}

	// 5. Provide the model credential by env-file (value never in argv).
	secretEnv := map[string]string{}
	if req.AuthValue != "" {
		secretEnv[envVar] = req.AuthValue
	}

	outDir := filepath.Join(req.WorkDir, "out")
	spec := runner.Spec{
		Name:       req.TaskID,
		Image:      deps.Image,
		SourceDir:  exportDir,
		PromptFile: promptPath,
		OutDir:     outDir,
		SecretEnv:  secretEnv,
		Cmd:        wrapperCmd(base, agentCmd),
		CPUs:       req.Limits.CPUs,
		MemoryMB:   req.Limits.MemoryMB,
		PidsLimit:  req.Limits.PidsLimit,
	}

	// 6. Enforce the wall-clock timeout via the run context (§7.4).
	runCtx := ctx
	if req.Limits.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Limits.Timeout)
		defer cancel()
	}

	deps.event(req.TaskID, store.EventPhase, "run agent")
	// The launch attempt is recorded before Run; success-side events after —
	// so the trail never claims a launch that failed (§12).
	deps.event(req.TaskID, store.EventSecurity, "launching container "+runner.ContainerName(spec))
	res, err := deps.Runner.Run(runCtx, spec)
	if err != nil {
		// The runner error may be the run context ending. Map the context cause
		// to the terminal state (§7.4): timeout is Failed (D9), a cancel is
		// Cancelled, a daemon shutdown is Failed. All fire the same cancel path.
		if state, reason, ok := ContextOutcome(runCtx); ok {
			return Outcome{
				State: state, Error: reason,
				Branch: branch, Worktree: wt.Path, OutDir: outDir,
				Artifacts: collectArtifacts(outDir),
			}, nil
		}
		return Outcome{}, fmt.Errorf("run agent container: %w", err)
	}
	// A successful Run means the container launched, ran, and was torn down
	// (teardown is deferred inside the runner).
	deps.event(req.TaskID, store.EventSecurity, "container launched")
	deps.event(req.TaskID, store.EventSecurity, "container destroyed")

	out = Outcome{
		ExitCode: res.ExitCode,
		Branch:   branch,
		Worktree: wt.Path,
		OutDir:   outDir,
	}
	out.Artifacts = collectArtifacts(outDir)

	// 7. Apply the bundle onto the feature branch, if the agent produced commits.
	bundlePath := filepath.Join(outDir, "changes.bundle")
	if fi, statErr := os.Lstat(bundlePath); statErr == nil {
		if !fi.Mode().IsRegular() {
			// podman cp preserves symlinks, so a hostile agent could point
			// changes.bundle at an arbitrary host path; never follow it.
			out.State = StateFailed
			out.Error = "rejected non-regular artifact changes.bundle"
			return out, nil
		}
		deps.event(req.TaskID, store.EventSecurity, "bundle extracted")

		deps.event(req.TaskID, store.EventPhase, "apply bundle")
		applied, applyErr := deps.Repo.ApplyBundle(ctx, wt.Path, bundlePath)
		if applyErr != nil {
			// An unappliable bundle is a task failure, not a pipeline error —
			// unless the task context ended (a cancel mid-apply is Cancelled).
			return failedOutcome(ctx, out, applyErr.Error()), nil
		}
		out.Commits = applied.Commits
	}

	// 8. Terminal outcome (D9): Completed iff the agent exited 0 AND ≥1 commit.
	if res.ExitCode != 0 || out.Commits < 1 {
		out.State = StateFailed
		return out, nil
	}
	out.State = StateCompleted

	// 9. Publish (Completed only): push the branch and open a PR (§9.3). A
	// publish failure downgrades the task to Failed with the reason; the commits
	// remain on the local feature branch for inspection.
	if gh != nil {
		if err := gh.Push(ctx, wt.Path, branch); err != nil {
			return failedOutcome(ctx, out, "push: "+err.Error()), nil
		}
		deps.event(req.TaskID, store.EventSecurity, "pushed branch "+branch)
		prTitle := title
		if prTitle == "" {
			prTitle = req.TaskID
		}
		body := github.BuildPRBody(github.PRInfo{
			TaskID: req.TaskID, Agent: req.Agent.Name(), IssueURL: issueURL,
			Summary: readArtifact(outDir, "summary.txt"),
		})
		url, err := gh.OpenPR(ctx, wt.Path, branch, req.DefaultBranch, prTitle, body)
		if err != nil {
			return failedOutcome(ctx, out, "open pr: "+err.Error()), nil
		}
		deps.event(req.TaskID, store.EventSecurity, "Opened PR "+url)
		out.PRURL = url
		if req.IssueNumber > 0 {
			// Best-effort back-link; a comment failure must not fail the task.
			_ = gh.CommentIssue(ctx, req.IssueNumber, "Agent-produced PR: "+url)
		}
	}
	return out, nil
}

// failedOutcome marks out Failed with reason — unless the task context ended,
// in which case the context's terminal state wins (cancel anywhere is
// Cancelled, timeout anywhere is Failed "timeout"; §7.4).
func failedOutcome(ctx context.Context, out Outcome, reason string) Outcome {
	out.State, out.Error = StateFailed, reason
	if state, r, ok := ContextOutcome(ctx); ok {
		out.State, out.Error = state, r
	}
	return out
}

// readArtifact returns the trimmed contents of outDir/name, or "" if absent or
// unreadable. Only regular files are read (never a symlinked artifact).
func readArtifact(outDir, name string) string {
	p := filepath.Join(outDir, name)
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// collectArtifacts classifies the files a run left in outDir. Only regular
// files count: podman cp preserves symlinks, so a symlinked artifact could
// alias arbitrary host files (Lstat does not follow links).
func collectArtifacts(outDir string) []Artifact {
	var arts []Artifact
	for _, a := range artifactKinds {
		p := filepath.Join(outDir, a.name)
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			arts = append(arts, Artifact{Kind: a.kind, Path: p})
		}
	}
	return arts
}

// event() and setState() are two best-effort functions a write should never fail.
// an unwritable event or state is a monitoring problem not a task failure
func (d Deps) event(taskID, typ, msg string) {
	if d.Recorder != nil {
		_ = d.Recorder.InsertEvent(taskID, typ, msg)
	}
}

func (d Deps) setState(taskID, state string) {
	if d.Recorder != nil {
		_ = d.Recorder.UpdateTaskState(taskID, state)
	}
}

func (d Deps) recordArtifacts(taskID string, arts []Artifact) {
	if d.Recorder == nil {
		return
	}
	for _, a := range arts {
		_ = d.Recorder.InsertArtifact(taskID, a.Kind, a.Path)
	}
}
