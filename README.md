# Agent-Task

Self-hosted platform for running coding agents (Claude Code, Pi) in isolated,
disposable containers. One task = one branch = one container; agents are
treated as untrusted code; a GitHub pull request is the only human approval
gate.

A single Go binary, `agent-task`, is both the always-on daemon (`serve`) and
the CLI you talk to it with, over a Unix socket — see `TECHNICAL_DESIGN.md`
for the full design and `docs/project/` for the underlying requirements.

> **Status:** single-user, self-hosted, under active development against the
> milestones in `TECHNICAL_DESIGN.md` (§12). Command flags and the socket
> protocol may still change between milestones.

## How it works

1. You submit a task: a GitHub issue number or free-form text, against a repo
   registered in `config.yaml`.
2. The daemon fetches the repo, creates a feature branch
   (`agent/<agent>/<slug>-<shortid>`), and copies a snapshot of the source
   into a disposable rootless-Podman container — the container never sees the
   real `.git` or a GitHub token.
3. The agent (Claude Code or Pi) runs non-interactively inside the container
   and commits its changes locally.
4. The host extracts the commits as a `git bundle`, applies them onto the
   feature branch, pushes with a repo-scoped token, and opens a PR.
5. You review and merge the PR — agents never merge or push directly.

## Requirements

- **Go** 1.26+ (to build `agent-task`)
- **Linux** with rootless Podman configured (subuid/subgid ranges, unprivileged
  user namespaces) — see `docs/runbook/0001-agent-task-vm.md` for a worked
  example of provisioning a host
- The **agent base image** built from `images/base/Dockerfile` (Node/TS + Bun,
  Go, Python 3, and the agent CLIs) — see `images/README.md`
- A GitHub **personal access token** (repo-scoped) if you want issue fetch,
  push, and PR creation; not required to just run an agent locally against a
  file:// repo
- A model credential for whichever agent you run: `CLAUDE_CODE_OAUTH_TOKEN`
  or `ANTHROPIC_API_KEY` for Claude Code; `ANTHROPIC_API_KEY` for Pi

## Install

Build the binary from source:

```bash
git clone https://github.com/iQonAi/agent-task.git
cd agent-task
go build -o agent-task ./cmd/agent-task
sudo install -m 0755 agent-task /usr/local/bin/agent-task
```

Run the test suite:

```bash
go test ./...
```

Build the agent base image (rootless, as the container-runner user — see
`images/README.md` for the full command and the smoke-test gate it runs):

```bash
cd images/base
sudo -u agentbox env HOME=/home/agentbox XDG_RUNTIME_DIR=/run/user/999 \
  podman build --dns 9.9.9.9 -t agent-task-base:dev .
```

The `sudo -u agentbox env …` prefix matters: the image must land in the
container-runner user's rootless storage, or the daemon's podman cannot see
it and tasks fail at container create. On a dev machine where you run podman
as yourself, a plain `podman build` is fine.

### Running the daemon as a systemd service

`deploy/systemd/agent-taskd.service` is the reference unit: a Unix-socket-only
daemon (no TCP), secrets delivered via `LoadCredential`, state in
`/var/lib/agent-task`. It expects:

- the binary at `/usr/local/bin/agent-task`
- a config at `/etc/agent-task/config.yaml`
- credential files under `/etc/agent-task/credentials/` matching each
  `token_ref` in the config (repo-scoped GitHub tokens, per-agent model keys)
- an `agent-taskd` service user and an `agentwork` group shared with the
  container-runner user

```bash
sudo mkdir -p /etc/agent-task/credentials
sudo cp config.example.yaml /etc/agent-task/config.yaml   # then edit it
sudo cp deploy/systemd/agent-taskd.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now agent-taskd
```

See `docs/runbook/0001-agent-task-vm.md` for the full host setup (Podman, the
egress deny-list, credential provisioning), and
`docs/runbook/0002-add-a-repo.md` for the procedure to register a new repo
(token, credential file, `LoadCredential`, config entry).

## Configuration

`config.yaml` registers the repos and agents the daemon is allowed to act on
(the registry doubles as the security allowlist — a task can only target a
repo listed here). Copy `config.example.yaml` and edit:

```yaml
socket_path: /run/agent-task/agent-task.sock
data_dir: /var/lib/agent-task

limits:
  max_concurrent: 2
  task_timeout: 30m

repos:
  - name: agent-task
    owner: iQonAi
    repo: agent-task
    default_branch: main
    token_ref: gh-token-agent-task     # LoadCredential secret name, not the token itself

image: localhost/agent-task-base:dev
podman: podman                    # dev; production uses the cross-user wrapper:
                                  #   "sudo -u agentbox /usr/local/sbin/agentbox-podman"
                                  # (the sudoers NOPASSWD rule matches only that path)

agents:
  claude:
    auth: subscription             # or api_key
    token_ref: claude-oauth-token
  pi:
    auth: api_key                  # pi has no non-interactive subscription auth
    token_ref: anthropic-api-key
```

## Usage

### Via the daemon (`serve` + CLI)

Start the daemon (reads `/etc/agent-task/config.yaml` by default, or
`--config PATH`):

```bash
agent-task serve --config /etc/agent-task/config.yaml
```

From another shell on the same host (the daemon listens on a Unix socket only —
no TCP; for remote use, SSH to the host over Tailscale and run the CLI there,
per the runbook), submit and track tasks:

```bash
# list repos registered in the config
agent-task repos

# queue a task against a GitHub issue
agent-task submit --repo agent-task --agent claude --issue 40
# -> prints a task id

# or queue free-form work instead of an issue
agent-task submit --repo agent-task --agent claude --task "Add a --verbose flag to ls"

# list tasks
agent-task ls

# show a task's state and event log
agent-task status <task-id>

# cancel a running task
agent-task cancel <task-id>
```

The client commands (`repos`, `ls`, `submit`, `status`, `cancel`) accept
`--socket PATH` to point at a non-default socket (default:
`/run/agent-task/agent-task.sock`); `serve` takes its socket from the
config's `socket_path`, and `run` uses no socket at all.

### Standalone (`run`, no daemon)

`agent-task run` executes a single task in-process, without a daemon —
useful for local testing against a `file://` repo or iterating on an agent
adapter:

```bash
agent-task run \
  --repo-url file:///path/to/local/repo \
  --task "Fix the typo in the README" \
  --agent claude \
  --auth subscription \
  --model-token-file /path/to/claude-oauth-token
```

Or against a registered repo, fetching the prompt from a GitHub issue
(needs a GitHub token, from `--github-token-file` or the `GH_TOKEN` env var):

```bash
agent-task run --config /etc/agent-task/config.yaml \
  --repo agent-task --issue 40 --agent claude \
  --github-token-file /path/to/gh-token
```

If `--model-token-file`/`--github-token-file` are omitted, the token is read
from the agent's usual environment variable (e.g. `CLAUDE_CODE_OAUTH_TOKEN`,
`ANTHROPIC_API_KEY`) or `GH_TOKEN`.

`run` prints the outcome (state, commit count, exit code, branch, worktree,
PR URL if one was opened, and artifact paths) and indexes the task in the
same SQLite store the daemon uses, so it shows up in `agent-task ls`.

Run `agent-task` with no arguments for the full command summary.

## Repository layout

See `TECHNICAL_DESIGN.md` §4 for the annotated package layout
(`internal/controller`, `internal/runner`, `internal/agent`,
`internal/github`, `internal/store`, …) and §12 for the milestone plan this
codebase is built against.

## Further reading

- `TECHNICAL_DESIGN.md` — architecture, decisions, and milestones
- `docs/project/` — vision, system architecture, and the security/threat model
- `docs/runbook/0001-agent-task-vm.md` — operator runbook for provisioning a host
- `docs/runbook/0002-add-a-repo.md` — how to add a repository and its credentials
- `images/README.md` — agent base image contents and build/update process
