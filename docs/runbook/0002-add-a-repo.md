# 0002 — Add a Repository (CLI walkthrough + manual fallback)

Operator procedure to register a new repository with a running `agent-taskd`
deployment, using the `agent-task repos`/`creds` CLI subcommands. A manual
fallback (editing files by hand) is included for environments where the CLI
can't run, or to understand what the CLI does under the hood.

Background on the token model (machine user, org resource owner, token
scoping) is in `0001-agent-task-vm.md` § "GitHub machine user + repo-scoped
tokens (issue #6)". This page is the how; that section is the why.

## Prerequisites

- The repo lives in the `iQonAi` org (fine-grained PATs can only target the
  token creator's own account or an org they belong to — see runbook 0001).
- `iQonAi-Bot` has **Write** access to the repo (collaborator or team).
- A fine-grained GitHub PAT has been minted for the repo and **approved** by
  an org owner (org `iQonAi` → Settings → Personal access tokens → Pending
  requests → Approve). See the "manual fallback" section below for the exact
  token settings. Without approval, `repos doctor`'s live-access check fails
  with a "Could not resolve to a Repository" error that looks like a wrong
  repo name — it's an unapproved token.

## 1. Register the repo: `agent-task repos add`

```bash
agent-task repos add --url https://github.com/iQonAi/<repo> --name <repo>
```

`--url` accepts `https://github.com/<owner>/<repo>` or `git@github.com:<owner>/<repo>`
(with or without a trailing `.git`). Equivalently, pass `--owner <owner> --repo <repo>`
instead of `--url` (the two are mutually exclusive; exactly one is required).

Flags:

| Flag               | Required | Default                     | Notes                                                        |
| ------------------ | -------- | ---------------------------- | ------------------------------------------------------------- |
| `--name`           | yes      | —                             | short name used elsewhere (`agent-task submit --repo <name>`) |
| `--url`             | one of   | —                             | mutually exclusive with `--owner`/`--repo`                    |
| `--owner`/`--repo`  | one of   | —                             | mutually exclusive with `--url`                                |
| `--default-branch` | no       | `main`                        |                                                                 |
| `--token-ref`       | no       | `gh-token-<name>`             | names the `LoadCredential` secret this repo will use           |
| `--dry-run`         | no       | `false`                       | validates and prints the change without writing `config.yaml` |
| `--config`          | no       | `/etc/agent-task/config.yaml` |                                                                 |

`repos add` refuses to run if `--name` already exists in the registry. On
success it writes `config.yaml` (keeping a `.bak` backup) and prints:

```
added repo "<name>" to /etc/agent-task/config.yaml (backup at /etc/agent-task/config.yaml.bak)
restart the daemon for this change to take effect
```

The restart happens in the next step (`creds set` restarts the daemon as part
of wiring the credential), so there's no need to restart manually here unless
you're adding the repo without also setting a new credential.

## 2. Store the token: `agent-task creds set`

```bash
agent-task creds set gh-token-<repo>
# paste the token, Enter, Ctrl-D
```

The secret is read from **stdin** by default (never a flag or positional
argument, so it never lands in shell history or `/proc/<pid>/cmdline`), or
from a file with `--from-file PATH`. A trailing newline is trimmed; an empty
secret is rejected (`secret is empty`).

Flags:

| Flag                | Default                                          | Notes                                              |
| ------------------- | ------------------------------------------------- | --------------------------------------------------- |
| `--from-file`        | (reads stdin)                                      | read the secret from this file instead of stdin     |
| `--credentials-dir`  | `/etc/agent-task/credentials`                     | directory the credential file is written into       |
| `--unit-file`        | `/etc/systemd/system/agent-taskd.service`         | systemd unit to add the `LoadCredential` line to    |
| `--socket`           | `/run/agent-task/agent-task.sock`                 | used only for the in-flight task check              |
| `--force`            | `false`                                            | restart the daemon even if tasks are running         |

`creds set` writes the credential file (root-owned, mode `0600`), adds a
`LoadCredential=<ref>:<path>` line to the unit if one isn't already there, and
then restarts the daemon itself — this is the operator's own action, so no
extra confirmation prompt. If tasks are currently `Created` or `Running`, the
restart is blocked and it prints which tasks are in flight; rerun with
`--force` to restart anyway, or restart manually later
(`sudo systemctl restart agent-taskd`). If nothing changed (identical
credential, line already present), it prints `no change: credential and unit
were already up to date` and skips the restart entirely.

## 3. Verify: `agent-task repos doctor`

```bash
agent-task repos doctor <repo>
```

Run with no argument to check every registered repo. `repos doctor` checks,
for the repo(s): the credential file exists, the systemd unit has the matching
`LoadCredential` line, and the token can actually reach the repo on GitHub. See
the failure-mode table below for the exact messages it prints and how to fix
each one.

## Manual fallback

Use this if the CLI isn't available, or to understand what it does under the
hood. All four steps below are required; a missed step surfaces as a failed
`repos doctor` check, not a config-load error.

### 1. Mint the token

As `iQonAi-Bot`, create one **fine-grained** PAT for the repo:

| Setting           | Value                                                    |
| ------------------ | --------------------------------------------------------- |
| Resource owner     | `iQonAi` (the org, not the bot's account)                 |
| Repository access  | only the one repo                                          |
| Permissions         | Contents R/W, Pull requests R/W, Issues R/W, Metadata R   |
| Expiration          | 90 days                                                    |

Then have an org owner **approve** it: org `iQonAi` → Settings → Personal
access tokens → **Pending requests** → Approve.

> **A pending token is inert.** It authenticates, but every repo — including
> its own — returns 404 / "Could not resolve to a Repository". The error looks
> like a wrong repo name; it is an unapproved token.

### 2. Write the credential file by hand

Naming convention: `gh-token-<repo>`. Paste the token straight into a
root-owned `0600` file (never into shell history):

```bash
sudo sh -c 'umask 077; tr -d "\n" > /etc/agent-task/credentials/gh-token-<repo>'
# paste the token, Enter, Ctrl-D
sudo chmod 0600 /etc/agent-task/credentials/gh-token-<repo>
```

### 3. Add the `LoadCredential=` line

The daemon reads secrets **only** from `$CREDENTIALS_DIRECTORY`, which systemd
populates from `LoadCredential=` lines. The file from step 2 does nothing
until the unit delivers it. Add one line per repo, either editing the
installed unit (keep `deploy/systemd/agent-taskd.service` in the repo in sync
— it is the source of truth) or via a drop-in (`sudo systemctl edit
agent-taskd`, under `[Service]`):

```ini
LoadCredential=gh-token-<repo>:/etc/agent-task/credentials/gh-token-<repo>
```

```bash
sudo systemctl daemon-reload
sudo systemctl restart agent-taskd
```

### 4. Add the config.yaml entry

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
config. Restart the daemon (or fold this into step 3's restart) and verify
with `agent-task repos`.

## Failure modes (`repos doctor`)

`repos doctor` runs three independent checks per repo and reports all three
regardless of earlier failures:

| Check              | `[ok]` message                                                | `[FAIL]` message                                                                                                                                                                 | Fix                       |
| -------------------- | ---------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------- |
| Credential file      | `credential file exists: <path>`                                 | `credential file missing: <path> — fix: agent-task creds set <ref>`                                                                                                                 | `agent-task creds set <ref>` |
| systemd unit wiring  | `systemd unit has a LoadCredential line for "<ref>"`              | `systemd unit missing a LoadCredential line for "<ref>" — fix: agent-task creds set <ref>`                                                                                         | `agent-task creds set <ref>` |
| Live GitHub access   | `live GitHub access to <owner>/<repo>`                            | `live GitHub access: skipped, no credential file to read a token from — fix: agent-task creds set <ref>` (no credential file) or `live GitHub access to <owner>/<repo>: <err> — repo unreachable with this token: could be a wrong/renamed repo OR a token pending org-owner approval; see docs/runbook/0001-agent-task-vm.md's pending-approval note, and verify the repo name/owner in config.yaml` | `agent-task creds set <ref>`, or check token approval / repo name in `config.yaml` |

## Rotation

Fine-grained tokens expire (90 days). To rotate: mint + approve a new token
(fallback step 1), then run `agent-task creds set <ref>` again (it overwrites
the credential file and restarts the daemon). The registry entry
(`config.yaml`) and unit's `LoadCredential=` line are unchanged — they
reference the file by name, not its contents.
