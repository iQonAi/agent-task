package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/iQonAi/agent-task/internal/agent"
	"github.com/iQonAi/agent-task/internal/client"
	"github.com/iQonAi/agent-task/internal/config"
	"github.com/iQonAi/agent-task/internal/controller"
	"github.com/iQonAi/agent-task/internal/creds"
	"github.com/iQonAi/agent-task/internal/daemon"
	"github.com/iQonAi/agent-task/internal/prompt"
	"github.com/iQonAi/agent-task/internal/repo"
	"github.com/iQonAi/agent-task/internal/runner"
	"github.com/iQonAi/agent-task/internal/store"
	"github.com/iQonAi/agent-task/internal/unitfile"
)

const defaultConfigPath = "/etc/agent-task/config.yaml"

// defaultCredentialsDir is the host-side source directory for systemd
// LoadCredential (D3): a file here becomes available inside the daemon's
// runtime CREDENTIALS_DIRECTORY once the unit is (re)started.
const defaultCredentialsDir = "/etc/agent-task/credentials"

// defaultUnitPath is the installed systemd unit that systemd actually loads
// (README.md, docs/runbook/0001-agent-task-vm.md): the operator copies
// deploy/systemd/agent-taskd.service here on install/upgrade. This command
// edits the installed copy, never the repo's versioned reference copy.
const defaultUnitPath = "/etc/systemd/system/agent-taskd.service"

func main() {
	// os.Args[0] is the porgram name; os.args[1] is the subcommand (if any).
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "repos":
		err = runRepos(os.Args[2:])
	case "creds":
		err = runCreds(os.Args[2:])
	case "ls":
		err = runLs(os.Args[2:])
	case "run":
		err = runRun(os.Args[2:])
	case "submit":
		err = runSubmit(os.Args[2:])
	case "cancel":
		err = runCancel(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if errors.Is(err, errRestartSkipped) {
		// The credential write already succeeded; this isn't a failure, but
		// a script needs a non-zero, non-1 signal that the restart didn't
		// happen, distinct from exit 1 (a real failure).
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-task: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `agent-task - coding agent orchestrator

usage:
	agent-task serve [--config PATH]	start the daemon (Unix socket)
	agent-task repos [list] [--socket PATH]	list registered repositories
	agent-task repos add (--url URL | --owner O --repo R) --name NAME [--default-branch B] [--token-ref REF] [--dry-run]	register a repo in config.yaml
	agent-task repos remove NAME [--yes]	remove a repo from config.yaml
	agent-task creds set REF [--from-file PATH] [--force]	write a credential and wire it into the systemd unit
	agent-task creds list [--config PATH]	show configured credential refs and their status
	agent-task ls [--socket PATH]	list tasks
	agent-task run --task TEXT --repo-url URL [--agent claude]	run an agent task locally (M3)
	agent-task submit --repo NAME --agent NAME (--task TEXT | --issue N)	queue a task on the daemon
	agent-task cancel ID [--socket PATH]	cancel a running task
	agent-task status ID [--socket PATH]	show a task's state + events
`)
}

// githubRefRE matches a GitHub repo URL in https:// or git@ (SSH) form, for
// `repos add --url`.
var githubRefRE = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:)([^/]+)/([^/]+?)(\.git)?/?$`)

// parseGitHubRef parses a GitHub repo URL into its owner and repo name.
func parseGitHubRef(s string) (owner, repo string, err error) {
	m := githubRefRE.FindStringSubmatch(s)
	if m == nil {
		return "", "", fmt.Errorf("not a recognized GitHub URL: %q", s)
	}
	return m[1], m[2], nil
}

// runSubmit queues a task on the daemon over the socket.
func runSubmit(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	socket := fs.String("socket", config.DefaultSocketPath, "daemon socket path")
	repoName := fs.String("repo", "", "registered repo name")
	agentName := fs.String("agent", "claude", "agent adapter: claude|pi|mock")
	taskText := fs.String("task", "", "free-form task text")
	issueNum := fs.Int("issue", 0, "GitHub issue number")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repoName == "" {
		return fmt.Errorf("--repo is required")
	}
	if (*taskText == "") == (*issueNum == 0) {
		return fmt.Errorf("exactly one of --task or --issue is required")
	}
	id, err := client.New(*socket).Submit(*repoName, *agentName, *taskText, *issueNum)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

// runCancel cancels a running task by id.
func runCancel(args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	socket := fs.String("socket", config.DefaultSocketPath, "daemon socket path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: agent-task cancel <id>")
	}
	if err := client.New(*socket).Cancel(fs.Arg(0)); err != nil {
		return err
	}
	fmt.Println("cancelling", fs.Arg(0))
	return nil
}

// runStatus shows a task's state and audit events.
func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	socket := fs.String("socket", config.DefaultSocketPath, "daemon socket path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: agent-task status <id>")
	}
	task, events, err := client.New(*socket).Status(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("id:     %s\nstate:  %s\nsource: %s\n\nevents:\n", task.ID, task.State, task.Source)
	for _, e := range events {
		fmt.Printf("  %s  %-8s %s\n", e.TS.Format(time.RFC3339), e.Type, e.Message)
	}
	return nil
}

// runRun executes a single agent task end-to-end (M3). Standalone, in-process;
// the daemon/worker-pool path is M5.
func runRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	repoName := fs.String("repo", "", "registered repo name (from config); or use --repo-url")
	repoURL := fs.String("repo-url", "", "source repo URL (file:// or https://); overrides the registry")
	defaultBranch := fs.String("default-branch", "", "default branch (default: registry value or 'main')")
	taskText := fs.String("task", "", "free-form task text (or use --issue)")
	issueNum := fs.Int("issue", 0, "GitHub issue number to render into the prompt (needs --repo + token)")
	agentName := fs.String("agent", "claude", "agent adapter: claude|pi|mock")
	authStr := fs.String("auth", "subscription", "auth method: subscription|api_key")
	tokenFile := fs.String("model-token-file", "", "file holding the model token; else inherit the agent's env var")
	ghTokenFile := fs.String("github-token-file", "", "file holding the repo-scoped GitHub token; else inherit GH_TOKEN")
	image := fs.String("image", "localhost/agent-task-base:dev", "agent base image")
	podman := fs.String("podman", "podman", "podman command, e.g. 'sudo -u agentbox /usr/local/sbin/agentbox-podman'")
	dataDir := fs.String("data-dir", "", "mirror cache dir (default: config data_dir)")
	workDir := fs.String("work-dir", "", "scratch dir for this run (default: a fresh temp dir)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *taskText == "" && *issueNum == 0 {
		return fmt.Errorf("one of --task or --issue is required")
	}
	if *taskText != "" && *issueNum != 0 {
		return fmt.Errorf("--task and --issue are mutually exclusive")
	}
	if *repoURL == "" && *repoName == "" {
		return fmt.Errorf("one of --repo or --repo-url is required")
	}

	rName, rURL, rBranch := *repoName, *repoURL, *defaultBranch
	rOwner, rRepo, rTokenRef := "", "", ""
	if *repoName != "" && *repoURL == "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		var found *config.Repo
		for i := range cfg.Repos {
			if cfg.Repos[i].Name == *repoName {
				found = &cfg.Repos[i]
				break
			}
		}
		if found == nil {
			return fmt.Errorf("repo %q is not in the registry", *repoName)
		}
		rURL = fmt.Sprintf("https://github.com/%s/%s.git", found.Owner, found.Repo)
		rOwner, rRepo, rTokenRef = found.Owner, found.Repo, found.TokenRef
		if rBranch == "" {
			rBranch = found.DefaultBranch
		}
	}
	if rName == "" {
		rName = "task-repo"
	}
	if rBranch == "" {
		rBranch = "main"
	}

	// The repo-scoped GitHub token (D3): from a file, else inherit GH_TOKEN. Used
	// host-only for the private clone, push, PR, and issue fetch.
	ghToken := os.Getenv("GH_TOKEN")
	if *ghTokenFile != "" {
		b, err := os.ReadFile(*ghTokenFile)
		if err != nil {
			return fmt.Errorf("read github token: %w", err)
		}
		ghToken = strings.TrimSpace(string(b))
	}
	if *issueNum > 0 && (ghToken == "" || rOwner == "") {
		return fmt.Errorf("--issue needs a GitHub token (--github-token-file/GH_TOKEN) and --repo")
	}

	ag, err := agent.Lookup(*agentName)
	if err != nil {
		return err
	}

	var authValue string
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("read model token: %w", err)
		}
		authValue = strings.TrimSpace(string(b))
	} else if envVar, verr := ag.EnvVar(agent.AuthMethod(*authStr)); verr == nil {
		// No token file: inherit from the agent's env var (e.g. the operator
		// exported CLAUDE_CODE_OAUTH_TOKEN), as the flag help promises.
		authValue = os.Getenv(envVar)
	}

	wd := *workDir
	if wd == "" {
		if wd, err = os.MkdirTemp("", "agent-run-"); err != nil {
			return err
		}
	}
	// The container runs as agentbox (a different uid), so it must read the
	// source/prompt and write the out dir. M3 uses world-accessible scratch
	// dirs; M5 replaces this with a shared group (see the runbook).
	for _, d := range []string{wd, filepath.Join(wd, "out")} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o777); err != nil {
			return err
		}
	}
	dd := *dataDir
	if dd == "" {
		if cfg, err := config.Load(*configPath); err == nil {
			dd = cfg.DataDir
		} else {
			dd = wd
		}
	}

	deps := controller.Deps{
		Repo:   repo.NewManager(dd),
		Runner: runner.NewPodmanRunner(strings.Fields(*podman), nil),
		Image:  *image,
	}
	req := controller.Request{
		TaskID:        fmt.Sprintf("t%d", time.Now().UnixNano()),
		Title:         *taskText, // "" for --issue; the controller uses the issue title
		RepoName:      rName,
		RepoURL:       rURL,
		Owner:         rOwner,
		Repo:          rRepo,
		DefaultBranch: rBranch,
		IssueNumber:   *issueNum,
		GitHubToken:   ghToken,
		Prompt:        prompt.Input{Task: *taskText},
		Agent:         ag,
		AuthMethod:    agent.AuthMethod(*authStr),
		AuthValue:     authValue,
		WorkDir:       wd,
		Limits:        controller.Limits{CPUs: "2", MemoryMB: 2048, PidsLimit: 256, Timeout: 30 * time.Minute},
	}

	// The deadline covers the whole task, matching the daemon path: a wedged
	// git/gh call is bounded too, not just the container run.
	runCtx, cancelRun := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancelRun()
	out, err := controller.Run(runCtx, deps, req)
	if err != nil {
		return err
	}

	fmt.Printf("state:    %s\n", out.State)
	fmt.Printf("commits:  %d\n", out.Commits)
	fmt.Printf("exit:     %d\n", out.ExitCode)
	fmt.Printf("branch:   %s\n", out.Branch)
	fmt.Printf("worktree: %s\n", out.Worktree)
	if out.PRURL != "" {
		fmt.Printf("pr:       %s\n", out.PRURL)
	}
	for _, a := range out.Artifacts {
		fmt.Printf("artifact: %-10s %s\n", a.Kind, a.Path)
	}

	// Index the run in the store (issue #11: artifacts captured and indexed).
	// Same DB the daemon opens; the standalone run and `ls` share it.
	st, err := store.Open(filepath.Join(dd, "agent-task.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.UpsertRepo(store.Repo{
		Name: rName, Owner: rOwner, Repo: rRepo,
		DefaultBranch: rBranch, TokenRef: rTokenRef,
	}); err != nil {
		return err
	}
	repos, err := st.ListRepos()
	if err != nil {
		return err
	}
	var repoID int64
	for _, r := range repos {
		if r.Name == rName {
			repoID = r.ID
			break
		}
	}
	if err := st.CreateTask(store.NewTask{
		ID: req.TaskID, RepoID: repoID, Source: "manual", Agent: *agentName,
		Branch: out.Branch, HostWorktree: out.Worktree,
	}); err != nil {
		return err
	}
	if err := st.UpdateTaskState(req.TaskID, out.State); err != nil {
		return err
	}
	for _, a := range out.Artifacts {
		if err := st.InsertArtifact(req.TaskID, a.Kind, a.Path); err != nil {
			return err
		}
	}
	if out.PRURL != "" {
		if err := st.SetTaskPRURL(req.TaskID, out.PRURL); err != nil {
			return err
		}
	}

	if out.State != controller.StateCompleted {
		if out.Error != "" {
			return fmt.Errorf("task did not complete (state=%s, exit=%d): %s", out.State, out.ExitCode, out.Error)
		}
		return fmt.Errorf("task did not complete (state=%s, exit=%d)", out.State, out.ExitCode)
	}
	return nil
}

// runServe parses the serve flags and runs the daemon until SIGINT/SIGTERM.
func runServe(args []string) error {
	// the global flag package can't express subcommands, so each one get its
	// own FlagSet parsing the args after teh subcommand name
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// structured logs to stderr; systemd captures them into the journal
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// Cancels ctx on the first SIGINT/SIGTERM. A second signal falls through to
	// the default handler, so a wedged daemon can still be killed.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return daemon.Run(ctx, cfg)
}

// runRepos dispatches to the repos subcommands. Bare `repos` (no args, or a
// flag meant for list) stays an alias for `repos list`, matching the
// pre-subcommand behavior exactly.
func runRepos(args []string) error {
	if len(args) == 0 {
		return runReposList(args)
	}
	switch args[0] {
	case "list":
		return runReposList(args[1:])
	case "add":
		return runReposAdd(args[1:])
	case "remove":
		return runReposRemove(args[1:])
	default:
		if strings.HasPrefix(args[0], "-") {
			return runReposList(args)
		}
		return fmt.Errorf("unknown repos subcommand %q (want list|add|remove)", args[0])
	}
}

// runReposList lists registered repositories, read from the daemon over the
// socket.
func runReposList(args []string) error {
	fs := flag.NewFlagSet("repos list", flag.ExitOnError)
	socket := fs.String("socket", config.DefaultSocketPath, "daemon socket path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	repos, err := client.New(*socket).Repos()
	if err != nil {
		return err
	}

	// tabwriter bufferes every row, then pads columns to the widest cell - which
	// is why nothing prints until Flush
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tOWNER\tREPO\tBRANCH\tTOKEN_REF")
	for _, r := range repos {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.Name, r.Owner, r.Repo, r.DefaultBranch, r.TokenRef)
	}
	return tw.Flush()
}

// runReposAdd registers a new repo in config.yaml. This is a one-shot CLI
// edit, not a daemon RPC (ADR 0014 amendment): the daemon only seeds the
// registry at startup, so a restart is needed for the change to take effect.
func runReposAdd(args []string) error {
	fs := flag.NewFlagSet("repos add", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	url := fs.String("url", "", "GitHub repo URL (https:// or git@), an alternative to --owner/--repo")
	owner := fs.String("owner", "", "GitHub repo owner")
	repoName := fs.String("repo", "", "GitHub repo name")
	name := fs.String("name", "", "short name for this repo in the registry (required)")
	defaultBranch := fs.String("default-branch", "main", "default branch")
	tokenRef := fs.String("token-ref", "", "LoadCredential secret name (default: gh-token-<name>)")
	dryRun := fs.Bool("dry-run", false, "validate and print the change without writing config.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *name == "" {
		return fmt.Errorf("--name is required")
	}

	o, r := *owner, *repoName
	if *url != "" {
		if o != "" || r != "" {
			return fmt.Errorf("--url and --owner/--repo are mutually exclusive")
		}
		var err error
		o, r, err = parseGitHubRef(*url)
		if err != nil {
			return err
		}
	}
	if o == "" || r == "" {
		return fmt.Errorf("one of --url or --owner/--repo is required")
	}

	ref := *tokenRef
	if ref == "" {
		ref = "gh-token-" + *name
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	for _, existing := range cfg.Repos {
		if existing.Name == *name {
			return fmt.Errorf("repo %q already exists in %s", *name, *configPath)
		}
	}

	entry := config.Repo{
		Name:          *name,
		Owner:         o,
		Repo:          r,
		DefaultBranch: *defaultBranch,
		TokenRef:      ref,
	}

	if *dryRun {
		fmt.Printf("dry-run: would add repo %q (%s/%s, branch=%s, token_ref=%s) to %s\n",
			entry.Name, entry.Owner, entry.Repo, entry.DefaultBranch, entry.TokenRef, *configPath)
		return nil
	}

	if err := config.AddRepo(*configPath, entry); err != nil {
		return err
	}
	fmt.Printf("added repo %q to %s (backup at %s.bak)\n", entry.Name, *configPath, *configPath)
	fmt.Println("restart the daemon for this change to take effect")
	return nil
}

// runReposRemove removes a repo from config.yaml. It does not touch
// credential files or the systemd unit, and the daemon needs a restart to
// pick up the change (ADR 0014 amendment).
func runReposRemove(args []string) error {
	fs := flag.NewFlagSet("repos remove", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: agent-task repos remove <name>")
	}
	name := fs.Arg(0)

	if !*yes {
		ok, err := confirm(fmt.Sprintf("remove repo %q from %s? [y/N] ", name, *configPath))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("aborted")
			return nil
		}
	}

	found, err := config.RemoveRepo(*configPath, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("repo %q not found in %s", name, *configPath)
	}

	fmt.Printf("removed repo %q from %s (backup at %s.bak)\n", name, *configPath, *configPath)
	fmt.Println("this does not touch credential files or the systemd unit")
	fmt.Println("restart the daemon for this change to take effect")
	return nil
}

// confirm prompts the user on stdin for a y/n answer.
func confirm(prompt string) (bool, error) {
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes", nil
}

// runCreds dispatches to the creds subcommands.
func runCreds(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: agent-task creds set <ref> | agent-task creds list")
	}
	switch args[0] {
	case "set":
		return runCredsSet(args[1:])
	case "list":
		return runCredsList(args[1:])
	default:
		return fmt.Errorf("unknown creds subcommand %q (want set|list)", args[0])
	}
}

// runCredsSet writes a secret to the credentials directory and wires it into
// the systemd unit's LoadCredential lines. The secret is read from stdin (the
// default) or --from-file; there is no flag or positional argument for the
// value itself, so it is never visible in the shell history or process list.
func runCredsSet(args []string) error {
	fs := flag.NewFlagSet("creds set", flag.ExitOnError)
	fromFile := fs.String("from-file", "", "read the secret from this file instead of stdin")
	credentialsDir := fs.String("credentials-dir", defaultCredentialsDir, "directory to write the credential file into")
	unitPath := fs.String("unit-file", defaultUnitPath, "systemd unit file to add the LoadCredential line to")
	socket := fs.String("socket", config.DefaultSocketPath, "daemon socket path (for the in-flight task check)")
	force := fs.Bool("force", false, "restart the daemon even if tasks are running")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: agent-task creds set <ref> [--from-file PATH]")
	}
	ref := fs.Arg(0)
	if err := creds.ValidateRef(ref); err != nil {
		return err
	}

	var src io.Reader = os.Stdin
	if *fromFile != "" {
		f, err := os.Open(*fromFile)
		if err != nil {
			return fmt.Errorf("open --from-file: %w", err)
		}
		defer f.Close()
		src = f
	}
	data, err := io.ReadAll(src)
	if err != nil {
		return fmt.Errorf("read secret: %w", err)
	}
	data = trimTrailingNewline(data)
	if len(data) == 0 {
		return fmt.Errorf("secret is empty")
	}

	credPath := filepath.Join(*credentialsDir, ref)
	fileChanged, err := writeCredentialFile(credPath, data)
	if err != nil {
		return err
	}

	unitChanged, err := unitfile.EnsureLoadCredential(*unitPath, ref, credPath)
	if err != nil {
		return err
	}

	fmt.Printf("wrote credential %q to %s\n", ref, credPath)
	if unitChanged {
		fmt.Printf("added LoadCredential line for %q to %s\n", ref, *unitPath)
	}

	if !fileChanged && !unitChanged {
		fmt.Println("no change: credential and unit were already up to date")
		return nil
	}

	return maybeRestart(*socket, unitChanged, *force)
}

// trimTrailingNewline strips a single trailing newline (and, if CRLF, the
// preceding carriage return) the way creds.Get trims what it reads back, but
// otherwise returns data unchanged.
func trimTrailingNewline(data []byte) []byte {
	if n := len(data); n > 0 && data[n-1] == '\n' {
		data = data[:n-1]
		if n := len(data); n > 0 && data[n-1] == '\r' {
			data = data[:n-1]
		}
	}
	return data
}

// writeCredentialFile writes data to path, reporting whether the content
// changed (false if an identical file already existed there). The caller
// uses this to decide whether a daemon restart is needed. The file ends up
// at mode 0600 either way: os.WriteFile only applies a mode when it creates
// the file, so an existing file with a looser mode (e.g. from before this
// command existed) needs an explicit chmod to be brought back into line.
func writeCredentialFile(path string, data []byte) (changed bool, err error) {
	existing, err := os.ReadFile(path)
	same := err == nil && bytes.Equal(existing, data)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read existing credential at %s: %w", path, err)
	}
	if !same {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return false, fmt.Errorf("write credential to %s: %w", path, err)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return false, fmt.Errorf("chmod credential at %s: %w", path, err)
	}
	return !same, nil
}

// taskLister is the subset of *client.Client that the in-flight check needs;
// tests substitute a fake to simulate running tasks or an unreachable daemon.
type taskLister interface {
	Tasks() ([]store.Task, error)
}

// inFlightTaskIDs returns the IDs of non-terminal (Created or Running)
// tasks, plus the raw error from Tasks() (nil on success). It does not
// decide what an error means -- restartBlockReason does that, since
// "daemon not listening at all" and "daemon up but not responding" call for
// different handling.
func inFlightTaskIDs(c taskLister) ([]string, error) {
	tasks, err := c.Tasks()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, t := range tasks {
		if t.State == store.StateCreated || t.State == store.StateRunning {
			ids = append(ids, t.ID)
		}
	}
	return ids, nil
}

// daemonUnreachable reports whether err indicates nothing is listening on
// the daemon socket at all: a dial failure (connection refused, or the
// socket path doesn't exist -- both surface as a *net.OpError with
// Op "dial"). Per issue #57's design note, that case has nothing running,
// so a restart is safe. Any other error -- including a timeout, which can
// mean the daemon accepted the connection but is hung -- is not treated as
// safe.
func daemonUnreachable(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// restartBlockReason returns why the self-service restart must not proceed
// without --force, or "" if it may proceed. tasksErr is the error (if any)
// from listing the daemon's tasks.
func restartBlockReason(running []string, tasksErr error) string {
	switch {
	case tasksErr != nil && daemonUnreachable(tasksErr):
		return "" // nothing listening at all: nothing running, safe
	case tasksErr != nil:
		return fmt.Sprintf("could not confirm the daemon has no in-flight tasks: %v", tasksErr)
	case len(running) > 0:
		return fmt.Sprintf("%d task(s) still running (%s)", len(running), strings.Join(running, ", "))
	default:
		return ""
	}
}

// errRestartSkipped signals that creds set wrote the credential
// successfully but declined to restart the daemon -- in-flight tasks, or an
// inconclusive in-flight check, blocked it without --force. The credential
// write is not a failure, so main() maps this to its own exit code rather
// than the generic failure exit code.
var errRestartSkipped = errors.New("restart skipped (see the message above); rerun with --force or restart manually")

// maybeRestart implements the self-service restart from issue #57's design
// note: it runs `systemctl restart agent-taskd` itself, as the operator's
// own action, rather than only printing a reminder. It checks the daemon's
// own task list first so it doesn't kill an in-flight task; if the check is
// blocked or inconclusive, it warns and requires --force. UX choice: when
// nothing is running it just restarts and informs the operator, with no
// extra confirmation prompt -- running `creds set` is already the
// operator's own explicit action. unitChanged controls whether it reloads
// the unit first: `systemctl restart` alone does not reparse a unit file
// that changed on disk, but a daemon-reload on every run (including a
// no-op) would be unnecessary. If systemctl itself fails (no systemd, or no
// permission -- expected in dev/test environments), it falls back to
// printing the manual-restart reminder instead of erroring the command out:
// the credential write already succeeded, which is the important part.
func maybeRestart(socketPath string, unitChanged, force bool) error {
	running, tasksErr := inFlightTaskIDs(client.New(socketPath))
	if reason := restartBlockReason(running, tasksErr); reason != "" && !force {
		fmt.Printf("%s; not restarting\n", reason)
		fmt.Println("rerun with --force to restart anyway, or restart manually later: sudo systemctl restart agent-taskd")
		return errRestartSkipped
	}

	if unitChanged {
		if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
			fmt.Println("could not reload the systemd unit automatically; reload and restart manually for this change to take effect:")
			fmt.Println("  sudo systemctl daemon-reload && sudo systemctl restart agent-taskd")
			if msg := strings.TrimSpace(string(out)); msg != "" {
				fmt.Printf("(systemctl said: %s)\n", msg)
			}
			return nil
		}
	}

	if out, err := exec.Command("systemctl", "restart", "agent-taskd").CombinedOutput(); err != nil {
		fmt.Println("could not restart agent-taskd automatically; restart it manually for this change to take effect:")
		fmt.Println("  sudo systemctl restart agent-taskd")
		if msg := strings.TrimSpace(string(out)); msg != "" {
			fmt.Printf("(systemctl said: %s)\n", msg)
		}
		return nil
	}

	fmt.Println("restarted agent-taskd")
	return nil
}

// runCredsList shows configured credential refs (names only, never values),
// cross-referenced against the credentials directory and the systemd unit's
// LoadCredential lines.
func runCredsList(args []string) error {
	fs := flag.NewFlagSet("creds list", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	credentialsDir := fs.String("credentials-dir", defaultCredentialsDir, "directory credential files are written to")
	unitPath := fs.String("unit-file", defaultUnitPath, "systemd unit file to check LoadCredential lines in")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	unitData, err := os.ReadFile(*unitPath)
	if err != nil {
		return fmt.Errorf("read unit file %s: %w", *unitPath, err)
	}
	unitText := string(unitData)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REF\tSTATUS")
	for _, ref := range configTokenRefs(cfg) {
		fmt.Fprintf(tw, "%s\t%s\n", ref, credStatus(*credentialsDir, unitText, ref))
	}
	return tw.Flush()
}

// configTokenRefs collects every token_ref named in config.yaml's repos and
// agents, deduplicated, repos first (in their config order) then agents
// (sorted by name for deterministic output).
func configTokenRefs(cfg *config.Config) []string {
	seen := make(map[string]bool)
	var refs []string
	for _, r := range cfg.Repos {
		if r.TokenRef != "" && !seen[r.TokenRef] {
			seen[r.TokenRef] = true
			refs = append(refs, r.TokenRef)
		}
	}
	agentNames := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		agentNames = append(agentNames, name)
	}
	sort.Strings(agentNames)
	for _, name := range agentNames {
		ref := cfg.Agents[name].TokenRef
		if ref != "" && !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	return refs
}

// credStatus reports ref's wiring status: "ok" if both the credential file
// and the unit's LoadCredential line exist, "missing file" if the file does
// not, else "missing unit line".
func credStatus(credentialsDir, unitText, ref string) string {
	_, err := os.Stat(filepath.Join(credentialsDir, ref))
	hasFile := err == nil
	hasLine := unitfile.HasLine(unitText, ref)

	switch {
	case hasFile && hasLine:
		return "ok"
	case !hasFile:
		return "missing file"
	default:
		return "missing unit line"
	}
}

func runLs(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	socket := fs.String("socket", config.DefaultSocketPath, "daemon socket path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	tasks, err := client.New(*socket).Tasks()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tREPO_ID\tSOURCE\tSTATE\tCREATED")
	for _, t := range tasks {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", t.ID, t.RepoID, t.Source, t.State, t.CreatedAt.Format(time.RFC3339))
	}
	return tw.Flush()
}
