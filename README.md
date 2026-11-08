# agent-kb

**One git-backed knowledge base that Claude Code, Codex, Hermes, Cursor and
Antigravity all read, write and cross-check.**

Every coding agent now has private memory (`~/.claude/projects/*/memory`,
`~/.codex/memories`, Antigravity Knowledge Items), and none of it is shared.
`agent-kb` puts shared memory where every agent already looks, the repository,
and keeps it honest:

- **Claims, not notes.** Each entry is one small Markdown file in
  [Open Knowledge Format v0.2](https://github.com/GoogleCloudPlatform/open-knowledge-format)
  that records who generated it, who verified it, which code it depends on and
  when it goes stale.
- **Cross-checked by a different model.** A claim from Claude stays
  `unverified` until Codex (or a human) confirms it against the code, and the
  other way round. Self-verification within a model family is rejected.
- **Staleness from git.** If the files a claim depends on change after it was
  last confirmed, it's flagged `stale` automatically.
- **Cross-review built in.** `kb review submit` turns a commit range into a
  review packet. Only a different model family can approve it.
- **Thin.** A ~20-line pointer in `AGENTS.md`/`CLAUDE.md`; everything else is
  pulled on demand with `kb search` (≈20 tokens per hit). No daemon, no
  database, no network listener. One static Go binary.

Read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the design review and
the reasoning behind each decision.

## Install

```bash
git clone https://github.com/xuehaipeng/agent-kb && cd agent-kb && make install   # installs kb into $(go env GOPATH)/bin
```

Requires Go 1.22+ and git. Cross-compile for the Hermes host with
`GOOS=linux GOARCH=amd64 go build -o kb ./cmd/kb`.

## Quick start

```bash
cd your-repo
kb init --install claude,codex,git-hooks
git add knowledge AGENTS.md CLAUDE.md .claude .codex && git commit -m "Add shared agent knowledge base"
```

This creates:

```
knowledge/
  kb.yaml           types, TTLs, model families, retired terms, extractor commands
  POINTER.md        the always-on pointer (imported by CLAUDE.md)
  index.md          generated root index; each type dir has its own index.md
  log.md            append-only audit log
  facts/ learnings/ decisions/ conventions/ reviews/
AGENTS.md           pointer block (Codex, Cursor, Antigravity read this)
CLAUDE.md           @knowledge/POINTER.md
.claude/skills/kb/  full command reference, loaded only when needed
.claude/settings.json  SessionStart / PreCompact / SessionEnd hooks
```

Tell Codex who it is (Claude Code is detected automatically):

```toml
# ~/.codex/config.toml
[shell_environment_policy]
set = { KB_ACTOR = "codex" }
```

## How agents use it

```bash
# before touching code
kb search "stream timeout" --file internal/gateway/stream.go
#  learnings/stream-timeout-must-forward-ctx-cancel [corroborated] Stream timeout must forward ctx cancel - ... (anchored: internal/gateway/stream.go)
kb show stream-timeout

# record something durable
kb add learning "Vitest deadlocks on Node 24 without --no-isolate" \
  -d "Worker pool deadlocks on Node 24.1 unless isolation is disabled." \
  -r "vitest.config.ts" -b "Repro: pnpm test hangs at RUN. Fix: --no-isolate or pin Node 22."

# another model family checks it
KB_ACTOR=codex kb verify vitest-deadlocks --confirm -e "reproduced on node 24.1; --no-isolate fixes it"

# cross-review
kb review submit "Refactor stream timeout propagation" -a "client cancel aborts upstream" --base main
KB_ACTOR=codex kb review list --for-me
KB_ACTOR=codex kb review show <id>
KB_ACTOR=codex kb review verdict <id> --changes -f "no test asserts upstream abort"
kb review revise <id> -m "added abort assertion"      # after committing the fix
```

### Trust states

| State | Meaning |
|---|---|
| `unverified` | proposed, not yet checked by another family (a hypothesis) |
| `corroborated` | confirmed by at least one *different* model family |
| `reviewed` | confirmed by a human (`--as human:<you>`) |
| `disputed` | another family says it's wrong; a human decides with `kb resolve` |
| `deprecated` | retracted, resolved as wrong, or superseded by a newer claim |
| `+stale` | past `stale_after`, or its `resources` changed in git since it was confirmed |

States are **derived** from the append-only `verified` list on every read and
never stored, so they can't drift. Families come from the actor string:
`cursor/claude-sonnet-4.5` is `anthropic`, `codex/gpt-5.2` is `openai`
(see `families` in `kb.yaml`).

### Entry format

```markdown
---
type: Learning
title: Stream timeout must forward ctx cancel upstream
description: SGLang worker stream keeps running unless the client ctx cancel is forwarded.
tags: [gateway, grpc]
status: stable
resources: [internal/gateway/**]
generated: {by: claude-code/opus-4.5, at: "2026-11-08T05:34:10Z", commit: 0e17780f6e0a}
verified:
  - by: codex/gpt-5.2
    at: "2026-11-08T06:02:41Z"
    result: confirmed
    commit: 0e17780f6e0a
    evidence: stream.go forwards ctx.Done(); go test ./internal/gateway passes
stale_after: "2026-12-08T06:02:41Z"
---
Seen while debugging leaked GPU slots: cancel was not propagated in stream.go.
```

Agents never edit these files by hand. `kb` appends verifications, and content
changes happen by superseding (`kb add ... --supersedes <id>`). `learnings/`
and `decisions/` are immutable, and the pre-commit hook enforces that.

## Capture without relying on the agent to remember

| Agent | Read path | Write path |
|---|---|---|
| Claude Code | `CLAUDE.md` imports the pointer; `kb` skill | `PreCompact`/`SessionEnd` hooks run `kb extract` in the background on the transcript; `SessionStart` prints reviews/disputes waiting for you |
| Codex | `AGENTS.md` pointer; `.codex/skills/kb` | `kb add` per the pointer; nightly `kb extract --scan codex` over `~/.codex/sessions` rollouts for this repo |
| Hermes (remote) | `kb` skill + git clone | `kb add`; `kb sync` on cron (pull, reindex, commit `knowledge/` only, push) |
| Cursor, Antigravity, GrokBot | `AGENTS.md` pointer | read-mostly; `kb add` optional |

`kb extract` condenses the new part of a transcript and pipes it to a cheap
model (`extract.commands` in `kb.yaml`, default `claude -p --model haiku`),
which returns at most five claims. They land as `unverified` with the session
as their cited source. Offsets make repeated runs incremental, and a recursion
guard stops the extractor's own session from triggering the hook again.

Hermes setup: `kb install hermes` writes the skill and prints the cron line.
On the host:

```bash
git clone <your repo> ~/proj && cd ~/proj
export KB_ACTOR=hermes/<model>        # e.g. hermes/claude-sonnet-4.5: family follows the model
crontab -e   # */15 * * * * cd ~/proj && kb sync >> ~/proj/knowledge/.kb/sync.log 2>&1
```

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

The server speaks MCP over stdio only and exposes five tools: `kb_search`,
`kb_show`, `kb_add`, `kb_verify` and `kb_review`.

## Maintenance

```bash
kb status          # counts, disputes, stale claims, reviews waiting, claims per producer
kb lint            # schema, timestamps, links, footnotes, secrets, retired terms, index drift
kb lint --staged   # + immutability of learnings/decisions (used by the pre-commit hook)
kb maintain        # index + pointer refresh + lint + status; safe for cron
```

Lint reports and never invents content. Add phrases that must no longer appear
(retired scripts, old service names) to `lint.retired_terms` in `kb.yaml`.

## Development

```bash
make test      # go vet + go test ./...
make build     # ./bin/kb
```

Layout: `internal/okf` (frontmatter round-trip), `internal/kb` (store, trust,
search, reviews, lint, index), `internal/cli`, `internal/mcp`,
`internal/extract`, `internal/install` (pointer, skill, hooks templates).

## Status

v0.1. The core (format, trust state machine, search, reviews, lint, index,
sync, MCP) is covered by tests. The integration points listed in
[ARCHITECTURE.md §5](docs/ARCHITECTURE.md#5-unverified-integration-details)
(Codex skill path and env vars, Hermes paths, Claude Code hook payload fields)
come from vendor docs and need checking against the versions you run.
