# agent-kb

**One knowledge base shared by Claude Code, Codex, Hermes, Cursor and
Antigravity, across all your projects.**

Every coding agent now has private memory (`~/.claude/projects/*/memory`,
`~/.codex/memories`, Antigravity Knowledge Items), and none of it is shared
between tools, machines or repos. `agent-kb` gives them one git-backed
knowledge base they all read and write through a small CLI, `kb`:

- **Grouped by project.** One hub (`~/.kb`) holds `projects/<id>/` for each
  project plus `global/` for knowledge that spans projects. `kb` works out the
  project from the current repo's git remote, so agents see the current project
  plus relevant global entries by default and can search `--all`.
- **Evolving, not append-only.** When an agent finds an entry stale or wrong,
  it fixes it in place (`kb update`, a new revision with history in git),
  re-confirms it, or retires it.
- **Trust you can read.** Each entry records who wrote it, what evidence backs
  it, which code it depends on and when it goes stale. One agent with evidence
  is enough (`verified`); a second model family (`cross-checked`) or a human
  (`reviewed`) is a stronger signal, not a gate.
- **Staleness from git.** If the files an entry depends on change after it was
  last confirmed, it's flagged `stale` automatically, even when you search
  from another project.
- **Docs stay the source of truth.** `kb search` also returns matching
  sections of the repo's own docs (`doc docs/design.md:42 (§ Retry policy)`),
  read live from the checkout. `kb add` refuses entries that just restate a
  doc section, so the knowledge base holds only what the docs don't: gotchas,
  cross-repo facts, environment quirks, undocumented decisions.
- **Thin.** A ~20-line pointer in each agent's user-level instructions; the
  rest is pulled on demand with `kb search` (≈20 tokens per hit). No daemon,
  no database, no network listener. One static Go binary.
- **Optional cross-review.** `kb review submit` turns a commit range into a
  review packet another agent can check.

Design reasoning: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Install

```bash
git clone https://github.com/xuehaipeng/agent-kb && cd agent-kb && make install   # installs kb into $(go env GOPATH)/bin
```

Requires Go 1.22+ and git. For the Hermes host: `GOOS=linux GOARCH=amd64 go build -o kb ./cmd/kb`.

## Quick start (multi-project hub)

```bash
kb hub init --remote git@github.com:<you>/kb-hub.git   # private repo for sharing; optional
kb install claude codex                                # user-wide: every repo, no files added to repos
```

That's all. `kb hub init` creates `~/.kb` (override with `KB_HOME`) as a git
repo, and `kb install` adds:

| Agent | What gets written |
|---|---|
| Claude Code | `~/.claude/CLAUDE.md` imports `~/.kb/POINTER.md`; `~/.claude/skills/kb/`; hooks in `~/.claude/settings.json` |
| Codex | pointer block in `~/.codex/AGENTS.md`; `~/.codex/skills/kb/` |
| Antigravity | pointer block in `~/.gemini/GEMINI.md` (`kb install antigravity`) |
| Cursor | paste `kb pointer` into Settings › Rules (no file to write) |

Tell Codex who it is (Claude Code is detected automatically):

```toml
# ~/.codex/config.toml
[shell_environment_policy]
set = { KB_ACTOR = "codex" }
```

Then work as usual in any repo, e.g. `~/GolandProjects/aiproxy`:

```bash
kb project                       # project: aiproxy (github.com/atlascloudteam/aiproxy)
kb search "retry idempotency"    # aiproxy + global
kb search "volcano scheduler" --all
kb project list                  # every project, entry counts, local checkouts
```

```
~/.kb/
  kb.yaml                  types, TTLs, model families, autocommit
  global/                  facts/ learnings/ decisions/ conventions/  (shared by every project)
  projects/
    aiproxy/   project.yaml (repos: [github.com/atlascloudteam/aiproxy])  facts/ learnings/ ...
    kubedl/    ...
    skyfable/  project.yaml (repos: [.../skyfable-backend, .../skyfable-web])
  index.md, log.md         generated / append-only
  .kb/                     machine-local (checkout registry, lock): gitignored
```

- A project spanning several repos, or a fork: `kb project link <id>` inside
  the repo.
- Every write is committed to the hub automatically. `kb sync` pulls and
  pushes, and the Claude Code hooks do it in the background at session
  start/end.
- A repo with its own `knowledge/kb.yaml` (repo mode: `kb init`) uses that
  bundle instead, for teams that want knowledge versioned with the code.
  `kb hub import <repo>/knowledge` moves it into the hub.

## How agents use it

```bash
# before working on unfamiliar code
kb search "stream timeout" --file internal/gateway/stream.go
#  aiproxy/learnings/stream-timeout-must-forward-cancel [verified] Stream timeout must forward cancel - ... (anchored: internal/gateway/stream.go)
kb show stream-timeout

#  doc docs/gateway.md:88 (§ Cancellation) - Client cancel is forwarded to the upstream stream.

# record something durable that the docs don't say, with how you know it
kb add learning "Vitest deadlocks on Node 24 without --no-isolate" \
  -d "Worker pool deadlocks on Node 24.1 unless isolation is disabled." \
  -r "vitest.config.ts" -e "reproduced: pnpm test hangs; --no-isolate fixes it" \
  -b "Repro: pnpm test hangs at RUN. Fix: --no-isolate or pin Node 22."
kb add convention "Wrap errors with %w" -d "All Go services." --global
kb add fact "aiproxy calls kubedl's job API with X-Tenant" -d "..." --global --for aiproxy,kubedl

# keep it current
kb update stream-timeout -b "Cancel is forwarded by forwardCancel() since v2.3." -m "refactor moved it" -e "stream.go:40"
kb verify stream-timeout --confirm -e "still true at HEAD: go test ./internal/gateway passes"
kb deprecate stream-timeout -m "gateway replaced by envoy"
```

### Trust states

| State | Meaning |
|---|---|
| `verified` | its author (or any agent of that model family) checked it and gave evidence |
| `cross-checked` | confirmed with evidence by a *different* model family |
| `reviewed` | confirmed by a human (`--as human:<you>`) |
| `unverified` | recorded without evidence: a lead to check |
| `disputed` | the latest check on this revision says it's wrong |
| `deprecated` | retracted, or superseded by a newer decision |
| `+stale` | past `stale_after`, or its `resources` changed in git since it was last confirmed |

States are **derived** from the append-only `verified` list on every read and
never stored. Every check records the content revision it looked at, so after
`kb update` the new content starts fresh and the old checks stay as history.
Families come from the actor string: `cursor/claude-sonnet-4.5` is
`anthropic`, `codex/gpt-5.2` is `openai` (see `families` in `kb.yaml`).

### Entry format

```markdown
---
type: Learning
title: Stream timeout must forward ctx cancel upstream
description: SGLang worker stream keeps running unless the client ctx cancel is forwarded.
tags: [gateway, grpc]
status: stable
resources: [internal/gateway/**]
revision: 2
generated: {by: claude-code, at: "2026-11-08T05:34:10Z", commit: 0e17780f6e0a}
updated: {by: codex/gpt-5.2, at: "2026-11-20T09:12:00Z", rev: 2, commit: 9c1d2e3f4a5b, evidence: refactor moved it}
verified:
  - by: claude-code
    at: "2026-11-08T05:34:10Z"
    result: confirmed
    rev: 1
    evidence: reproduced leaked GPU slots
  - by: codex/gpt-5.2
    at: "2026-11-20T09:12:00Z"
    result: confirmed
    rev: 2
    commit: 9c1d2e3f4a5b
    evidence: stream.go:40
stale_after: "2027-01-19T09:12:00Z"
---
Cancel is forwarded by forwardCancel() since v2.3.
```

These are [Open Knowledge Format v0.2](https://github.com/GoogleCloudPlatform/open-knowledge-format)
files with a few extension keys (`resources`, `revision`, `updated`,
`projects`, `verified[].rev/result/commit/evidence`). Agents never edit them by
hand. Decisions change by superseding (`kb add decision ... --supersedes <id>`)
so the history of a decision stays readable; everything else evolves with
`kb update`.

## Capture without relying on the agent to remember

| Agent | Read path | Write path |
|---|---|---|
| Claude Code | user-level `CLAUDE.md` imports the pointer; `kb` skill | `PreCompact`/`SessionEnd` hooks run `kb extract` in the background, then `kb sync`; `SessionStart` prints reviews, disputes and stale entries, and pulls the hub |
| Codex | `~/.codex/AGENTS.md` pointer; `kb` skill | `kb add` per the pointer; nightly `kb extract --scan codex` routes each session in `~/.codex/sessions` to its project |
| Hermes (remote) | `kb` skill + `kb hub clone` | `kb add -p <project>`; `kb sync` on cron |
| Cursor, Antigravity, GrokBot | pointer | `kb add` optional |

`kb extract` condenses the new part of a transcript and pipes it to a cheap
model (`extract.commands` in `kb.yaml`, default `claude -p --model haiku`),
which returns at most five entries. They land as `unverified` with the session
as their cited source. Offsets make repeated runs incremental, and a recursion
guard stops the extractor's own session from re-triggering the hook.

### Hermes host

```bash
kb hub clone git@github.com:<you>/kb-hub.git      # into ~/.kb
kb install hermes                                  # writes ~/.hermes/skills/kb/SKILL.md
export KB_ACTOR=hermes/<model>                     # family follows the model, e.g. hermes/claude-sonnet-4.5
crontab -e   # */15 * * * * KB_ACTOR=hermes kb sync >> ~/.kb/.kb/sync.log 2>&1
```

From Hermes: `kb search "..." --all`, `kb status --all`, `kb review list --all`,
`kb add learning "..." -d "..." -p kubedl`.

## MCP (optional)

The CLI is the primary interface because it costs no context until it's used.
For clients that prefer tools:

```bash
claude mcp add kb -- kb mcp --as claude-code
```

```toml
# ~/.codex/config.toml
[mcp_servers.kb]
command = "kb"
args = ["mcp", "--as", "codex"]
```

Six tools over stdio only: `kb_search`, `kb_show`, `kb_add`, `kb_update`,
`kb_verify`, `kb_review`.

## Cross-review (optional)

```bash
kb review submit "Refactor stream timeout propagation" -a "client cancel aborts upstream" --base main
KB_ACTOR=codex kb review list --for-me
KB_ACTOR=codex kb review show <id>          # packet, diffstat, knowledge anchored to the touched files
KB_ACTOR=codex kb review verdict <id> --changes -f "no test asserts upstream abort"
kb review revise <id> -m "added abort assertion"      # after committing the fix
```

Reviews by the author's own model family are allowed and labelled as such.

## Maintenance

```bash
kb status [--all]  # scope, counts, disputes, stale entries, open reviews, unsynced changes
kb lint            # schema, timestamps, links, footnotes, secrets, retired terms, index drift
kb lint --staged   # + immutability of decisions (used by the pre-commit hook)
kb maintain        # index + pointer refresh + lint + status; safe for cron
```

## Development

```bash
make test      # go vet + go test ./...
make build     # ./bin/kb
```

Layout: `internal/okf` (frontmatter round-trip), `internal/kb` (store, hub and
projects, trust, search, reviews, lint, index), `internal/cli`,
`internal/mcp`, `internal/extract`, `internal/install` (pointer, skill, hooks
templates).

## Status

v0.2. The core (format, trust model, revisions, hub and projects, search,
reviews, lint, index, sync, MCP) is covered by tests, including a two-machine
hub sync. The integration points listed in
[ARCHITECTURE.md §6](docs/ARCHITECTURE.md#6-unverified-integration-details)
(Codex skills path and env vars, Hermes paths, Claude Code hook payload and
`@~/` imports) come from vendor docs and need checking against the versions
you run.
