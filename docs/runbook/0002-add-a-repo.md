# 0002 — Add a Repository (config + credentials)

Operator procedure to register a new repository with a running `agent-taskd`
deployment. Four steps are required; **all four** must land before an `--issue`
task against the repo can run. A missed step fails at task time, not at config
load — see the failure table at the end.

Background on the token model (machine user, org resource owner, D3/D11) is in
`0001-agent-task-vm.md` § "GitHub machine user + repo-scoped tokens (issue #6)".
This page is the checklist; that section is the why.

## Prerequisites

- The repo lives in the `iQonAi` org (fine-grained PATs can only target the
  token creator's own account or an org they belong to — see runbook 0001).
- `iQonAi-Bot` has **Write** access to the repo (collaborator or team).

## 1. Mint the token

As `iQonAi-Bot`, create one **fine-grained** PAT for the repo:

| Setting           | Value                                          |
| ----------------- | ---------------------------------------------- |
| Resource owner    | `iQonAi` (the org, not the bot's account)      |
| Repository access | only the one repo                              |
| Permissions       | Contents R/W, Pull requests R/W, Issues R/W, Metadata R |
| Expiration        | 90 days                                        |

Then have an org owner **approve** it: org `iQonAi` → Settings → Personal
access tokens → **Pending requests** → Approve.

> **A pending token is inert.** It authenticates, but every repo — including
> its own — returns 404 / "Could not resolve to a Repository". The error looks
> like a wrong repo name; it is an unapproved token.

## 2. Store the credential file

Naming convention: `gh-token-<repo>`. Paste the token straight into a
root-owned `0600` file (never into shell history):

```bash
sudo sh -c 'umask 077; tr -d "\n" > /etc/agent-task/credentials/gh-token-<repo>'
# paste the token, Enter, Ctrl-D
sudo chmod 0600 /etc/agent-task/credentials/gh-token-<repo>
```

## 3. Wire it into the systemd unit

The daemon reads secrets **only** from `$CREDENTIALS_DIRECTORY`, which systemd
populates from `LoadCredential=` lines. The file from step 2 does nothing
until the unit delivers it. Add one line per repo:

```ini
LoadCredential=gh-token-<repo>:/etc/agent-task/credentials/gh-token-<repo>
```

Either edit the installed unit (keep `deploy/systemd/agent-taskd.service` in
the repo in sync — it is the source of truth) or use a drop-in
(`sudo systemctl edit agent-taskd`, under `[Service]`). Then:

```bash
sudo systemctl daemon-reload
sudo systemctl restart agent-taskd
```

Confirm delivery:

```bash
sudo systemctl show agent-taskd -p LoadCredential
```

## 4. Register the repo in the config

Add a block to `/etc/agent-task/config.yaml` (the registry doubles as the
security allowlist — a task can only target a repo listed here):

```yaml
repos:
  - name: <repo>            # the name used in `agent-task submit --repo <name>`
    owner: iQonAi
    repo: <repo>
    default_branch: main
    token_ref: gh-token-<repo>   # names the LoadCredential entry from step 3
```

`token_ref` is a **name**, not a secret — the token never appears in the
config. Restart the daemon (or fold this into step 3's restart) and verify:

```bash
agent-task repos
```

## Verify end to end

Test the token exactly as the daemon uses it (via `GH_TOKEN`), independent of
the daemon:

```bash
sudo sh -c 'GH_TOKEN=$(cat /etc/agent-task/credentials/gh-token-<repo>) \
  gh repo view iQonAi/<repo> --json name'
```

Then submit a real task:

```bash
agent-task submit --repo <repo> --agent claude --issue <n>
agent-task status <task-id>
```

## Failure modes

Every missed step surfaces as an instant task failure with a distinct message:

| Symptom (in `agent-task status`)                              | Cause                                                    | Fix    |
| ------------------------------------------------------------- | -------------------------------------------------------- | ------ |
| `unknown repo "<name>"` at submit                              | repo not in `config.yaml`, or daemon not restarted       | step 4 |
| `--issue requires a GitHub token and owner/repo`               | no `LoadCredential` entry — the credential resolves empty (a missing credential is deliberately not an error; see `internal/creds`) | step 3 |
| `credential "<ref>" is empty`                                  | credential file exists but is empty                      | step 2 |
| `Could not resolve to a Repository` (GraphQL, from `gh`)       | token cannot see the repo: not approved by the org, repo missing from the token's scope, or the bot lacks repo access | step 1 |

## Rotation

Fine-grained tokens expire (90 days). To rotate: mint + approve a new token
(step 1), overwrite the credential file (step 2), `sudo systemctl restart
agent-taskd`. Steps 3 and 4 are unchanged — they reference the file by name.
