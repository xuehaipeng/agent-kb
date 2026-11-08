---
name: kb
description: Shared knowledge base across agents, sessions and projects (kb CLI). Use before working on unfamiliar code to look up facts, gotchas, decisions and procedures; to record what you learned; to fix stale entries; and for optional cross-agent reviews.
---

# kb: shared knowledge for coding agents

`kb` manages small Markdown entries shared by Claude Code, Codex, Hermes and
other agents. In a hub (`~/.kb`), knowledge is grouped by project: the current
repo's project is detected from its git remote, and `global` holds what spans
projects. A repo with its own `knowledge/` directory uses that instead.

## Retrieve (cheap, do this first)

```bash
kb search "timeout propagation" --file internal/gateway/stream.go   # this project + global
kb search --file internal/gateway/stream.go                        # everything anchored to a file
kb search "rate limit tenant" --all                                # every project
kb search "..." -p aiproxy                                         # a specific project
kb show learnings/stream-timeout-must-propagate                    # full entry + trust trail
```

States: `verified` (author checked it), `cross-checked` (another model family
confirmed), `reviewed` (a human confirmed), `unverified` (a lead), `disputed`
(latest check says wrong), `stale` (past `stale_after`, or the files in
`resources` changed since it was last confirmed).

## Record

Record durable, non-obvious knowledge: root causes, gotchas, invariants,
decisions with reasons, procedures. Skip what the code or git log already says.
Give `-e` when you know it from evidence (a test you ran, code you read) so it
is `verified` right away; leave it out for hunches.

```bash
kb add learning "Vitest deadlocks on Node 24 without --no-isolate" \
  -d "Worker pool deadlocks on Node 24.1 unless isolation is disabled." \
  -r "vitest.config.ts" -t testing,vitest -e "reproduced: pnpm test hangs; --no-isolate fixes it" \
  -b "Repro: pnpm test on Node 24.1 hangs at RUN. Fix: --no-isolate or pin Node 22."
kb add convention "Errors wrap with %w and a lowercase message" -d "..." --global
kb add fact "aiproxy calls kubedl's job API with X-Tenant" -d "..." --global --for aiproxy,kubedl
kb add decision "Use Drizzle instead of Prisma" -d "..." --supersedes decisions/use-prisma
```

Types: `fact` (invariant or architecture fact), `learning` (gotcha, root cause),
`decision` (ADR; changes by superseding so history stays readable),
`convention` (team rule). One claim per entry; `-r` globs power path search and
staleness. If `kb add` reports a duplicate or a related entry, update that one.

## Keep it current

Knowledge evolves. When an entry is stale or wrong, fix it rather than adding
a near-duplicate:

```bash
kb update <id> -b "Port is 9090 since the ingress change." -m "port changed" -e "main.go:12"
kb update <id> -r "pkg/gateway/**" -m "code moved"
kb verify <id> --confirm -e "still true: stream_test.go passes at HEAD"
kb verify <id> --dispute -e "watermark.go no longer calls inc_lock_ref"
kb deprecate <id> -m "feature removed in v3"
```

Each update is a new revision (history in git); earlier verifications stay in
the trail for the revision they checked.

## Projects (hub mode)

```bash
kb project                 # which project this repo maps to
kb project list            # all projects with counts and local checkouts
kb project link skyfable   # group this repo (e.g. a frontend or a fork) under a project
kb status --all            # health across every project
```

## Optional cross-review

```bash
kb review submit "Refactor stream timeout propagation" -m "..." -a "cancel aborts upstream" --base main
kb review list --for-me
kb review show <id>          # packet, diffstat, knowledge anchored to the touched files
kb review verdict <id> --approve -m "..." | --changes -f "finding" -f "..."
kb review revise <id> -m "added abort assertion"   # author, after committing fixes
```

## Rules

- Never edit knowledge files by hand; never store secrets, tokens or personal data.
- Writes are attributed via `KB_ACTOR` (auto-detected for Claude Code); set
  `KB_ACTOR=<agent>/<model>` if `kb` asks.
