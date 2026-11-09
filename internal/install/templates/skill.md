---
name: kb
description: Shared knowledge base across agents, sessions and projects (kb CLI). Use before working on unfamiliar code to look up facts, gotchas, decisions and procedures; to record what you learned; to fix stale entries; and for optional cross-agent reviews.
---

# kb: shared knowledge for coding agents

`kb` manages small Markdown entries shared by Claude Code, Codex, Hermes and
other agents. In a hub (`~/.kb`), knowledge is grouped by project: the current
repo's project is detected from its git remote, and `global` holds what spans
projects. A repo with its own `knowledge/` directory uses that instead.

## Rules (always on)

Conventions are rules: they are pushed into every session instead of waiting
to be searched. Claude Code receives them at session start; other agents run
`kb rules` when starting a task. Follow them unless the user says otherwise.

```bash
kb rules                                   # rules for this project + global, and proposed ones
kb add rule "Use pnpm, never npm" -d "lockfile is pnpm-lock.yaml" -e "user said so"
kb add rule "Write commit messages in English" -d "team convention" -e "user said so" --global
kb rules import                            # share rules written for one agent (CLAUDE.md, .cursor/rules, ~/.codex/AGENTS.md ...)
```

Only evidence-backed rules are delivered. A rule you inferred yourself (no
`-e`) waits as "proposed" until the user confirms it, so one agent's guess
does not steer every other agent. Ask the user, then
`kb verify <id> --confirm -e "user confirmed"`.

## Retrieve (cheap, do this first)

```bash
kb search "timeout propagation" --file internal/gateway/stream.go   # this project + global
kb search --file internal/gateway/stream.go                        # everything anchored to a file
kb search "rate limit tenant" --all                                # every project
kb search "..." -p aiproxy                                         # a specific project
kb show learnings/stream-timeout-must-propagate                    # full entry + trust trail
```

Search also lists matching sections of the repository's own docs (README,
`docs/`, ADRs, `AGENTS.md`) as `doc path:line (§ heading)`. Read them directly:
they are the source of truth for what they cover, and kb never copies them.
Use `--no-docs` to see entries only.

States: `verified` (author checked it), `cross-checked` (another model family
confirmed), `reviewed` (a human confirmed), `unverified` (a lead), `disputed`
(latest check says wrong), `stale` (past `stale_after`, or the files in
`resources` changed since it was last confirmed).

## Record

Record durable, non-obvious knowledge: root causes, gotchas, invariants,
decisions with reasons, procedures. Skip what the code, the git log or the
repo's docs already say. The docs are the source of truth for what they
cover: `kb add` refuses an entry that restates a doc section (it names the
section), and warns on partial overlap. If a doc is wrong or missing something,
fix the doc instead of recording a correction here. The knowledge base is for
what the docs don't hold: gotchas found while working, cross-repo facts,
undocumented decisions, environment quirks.
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
`convention` (a rule or preference; `rule` is an alias; always on). One claim per entry; `-r` globs power path search and
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

## Seed what you already know

When asked to share your memories (or what you learned in earlier sessions)
about some projects, seed the durable ones in one call:

1. `kb project list` shows the project ids (inside a repo, `kb project`).
2. Keep what is durable, non-obvious and not in the repo's docs: gotchas, root
   causes, decisions and the user's rules or preferences. Skip notes about one
   finished task, personal data and secrets.
3. Write them as a JSON array (or JSON lines); each item may set `project` or
   `"global": true`, otherwise `-p` applies:

```bash
kb add --from - -e "from my claude-code memory" <<'JSON'
[{"type":"learning","project":"aiproxy","title":"Upstream 429 storms need jittered backoff",
  "description":"Retries without jitter amplify throttling.","resources":["pkg/retry/**"]},
 {"type":"rule","project":"kubedl","title":"Create the volcano queue before submitting jobs","description":"Jobs pend forever otherwise."},
 {"type":"rule","global":true,"title":"Keep PR descriptions short","description":"User preference."}]
JSON
```

Each item gets the usual checks; duplicates, restated docs and unknown project
ids are reported per item. A project with no entries yet needs `--new-project`
(this guards against typos creating stray projects). `-e` marks the items as
verified: use it only for things you actually know.

## Agents' own memories

What agents save in their private memory (Claude Code auto memory, Codex
memories, Hermes MEMORY.md/USER.md, Antigravity knowledge items) is imported
so every agent can use it: `kb extract --scan memories`, or only for some
projects: `kb extract --scan memories -p aiproxy -p kubedl`. Claude Code's is
imported automatically at session end; files are only re-read when they change.

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
