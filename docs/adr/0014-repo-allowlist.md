# ADR 0014 — Repo resolution: static allowlist in config, doubling as the security allowlist (D11)

**Status:** Accepted (approved 2026-06-18). **Amendment proposed:** ADR 0003
(D13, Proposed) keeps the registry as the allowlist but makes a repo an
*optional session property* and drops the ≥1-repo config requirement — not
yet accepted.

> **Amendment (2026-10-08):** an operator-run CLI may now write the
> registry and credential files directly, replacing manual file edits. The
> daemon's read-only relationship to the registry is unchanged. See
> "Amendment: operator CLI for registration" below.

## Context

Tasks name repos by short name (`--repo devbox`), so something must map
names to `owner/repo`, default branch, and credentials. The design folds
two concerns into one structure: the *identity mapping* and the *security
boundary*. Because each registry entry binds a repo to its own
fine-grained, repo-scoped token (D3/ADR 0007;
`TECHNICAL_DESIGN.md:321`), the registry is simultaneously the complete
list of what the platform can touch on GitHub — a repo absent from the
registry is unreachable, with no code path that could reach it.

## Decision

- The registry is a static list in `config.yaml`: per entry `name`,
  `owner`, `repo`, `default_branch`, and `token_ref` — the LoadCredential
  secret *name*, never the token itself
  (`internal/config/config.go:50-58`, `config.example.yaml:11-16`).
- Validation is strict at load: at least one repo (the current global
  requirement ADR 0003 proposes dropping —
  `internal/config/config.go:116-118`), unique names, owner/repo present,
  and `token_ref` required per entry
  (`internal/config/config.go:128-141`).
- Every task path resolves through the registry and rejects unknown
  names: the daemon's submit (`internal/daemon/submit.go:37-46`) and the
  standalone CLI run (`cmd/agent-task/main.go:180-189`). There is no
  ad-hoc URL path in the daemon — `--repo-url` exists only on the
  standalone `run` command for local development
  (`cmd/agent-task/main.go:147`).
- The config file stays the source of truth: on startup the daemon seeds
  the registry into the store (`internal/daemon/daemon.go:182-197`), and
  the DB row carries the same `token_ref` name, never a secret
  (`internal/store/migrations/0001_init.sql:3-11`).
- Registration is a deliberate operator action: edit config, provision the
  matching repo-scoped token via `LoadCredential`
  (`deploy/systemd/agent-taskd.service:37-46`), restart. There is no API
  to add a repo (superseded — see the 2026-10-08 amendment below).

## Consequences

- Blast radius is enumerable by reading one file: the platform can write
  to exactly the registered repos, each with its own least-privilege
  token. `agent-task repos` shows the live list
  (`cmd/agent-task/main.go:384-405`).
- Adding a repo costs an operator round-trip (config + token + restart) —
  friction that is the control working: nothing an agent produces can
  extend the allowlist.
- Task admission and secret resolution stay decoupled from GitHub: an
  unknown repo fails at submit time host-side, before any container or
  network activity (`internal/daemon/submit.go:44-46`).
- ADR 0003 (Proposed) shifts the registry's *coupling*, not its role: a
  repo becomes an optional property a session declares, but any declared
  repo must still resolve against this registry — no registry hit, no
  repo-bound work.

## Amendment: operator CLI for registration (2026-10-08)

The control in the original Decision was never the raw effort of
hand-editing YAML — it was requiring a deliberate, out-of-band human
action that nothing automated, including an agent, could trigger. An
operator-run CLI preserves that exact property: it still requires an
operator to deliberately invoke it. What it removes is the drudgery of
hand-editing `config.yaml` and the systemd unit's `LoadCredential`
entries, not the deliberateness requirement. Registration is moving from
hand-edited files to tooling on that basis (issues #56–#59):

- The new commands (`agent-task repos add/remove`, `agent-task creds
  set/list`, `agent-task repos doctor`) are a **one-shot CLI**, not a
  daemon socket RPC: each run is a local process that edits
  `config.yaml` and the credential source files under
  `/etc/agent-task/credentials/` directly, then exits. It does not talk
  to the running daemon to perform the write.
- The daemon gains **no new write path**. At startup it loads
  `config.yaml` once and seeds it into the SQLite store
  (`internal/daemon/daemon.go:182-197`, `seedRepos`, which itself only
  calls `store.UpsertRepo` — it does not touch credential files).
  Credentials are read per task, not at startup, via `creds.Get`
  (`internal/daemon/submit.go:59,63`); the daemon never writes a
  credential. The property this ADR's Decision relies on — no code path
  the platform runs can extend the allowlist — is unchanged, because the
  daemon's own capabilities are unchanged; only the operator's editing
  workflow changes.
- As with the original Decision's manual flow, an edit has no effect on
  a running daemon until restart: `seedRepos` only runs at startup, so
  `agent-task repos add/remove` and `agent-task creds set/list` still
  require an operator-initiated restart to take effect, exactly as
  hand-editing the files did before this amendment.
- Credentials stay operator-minted. The CLI tooling validates a token the
  operator already created (scope, repo match) and wires its reference
  into `config.yaml` and `LoadCredential`; it does not call the GitHub
  API to mint or request a token on the operator's behalf.
- This amendment is the approval gate for the CLI work: #56 (`repos
  add/remove`), #57 (`creds set/list`), #58 (`repos doctor`), and #59
  (runbook update) all depend on this decision and should not land ahead
  of it.
- This amendment authorizes only the one-shot CLI described above. A
  daemon-side write RPC (e.g. a socket command that lets the daemon
  itself mutate the registry or credentials) is explicitly not decided
  here and would need its own ADR and security review before any code
  implements it.
