# AgentKB architecture

This document reviews the two input designs, picks an architecture, and
records every place the implementation deviates from them and why.

Inputs:

- **Doc A**: *Build a git-backed knowledge bundle agents push into*, plus its
  2026-11-08 addendum (Claude Code and Codex primary, Hermes remote, goal is
  cross-checking facts and cross-reviewing tasks, retrieval must stay thin).
- **Doc B**: *Technical Design & Implementation Plan: AgentKB* (single binary,
  `kb_search` / `kb_fetch` / `kb_propose` / `kb_verify` / `kb_review`,
  code-anchored staleness, bilateral review packets, Tailscale or git sync for
  Hermes).
- A ChatGPT share link was also supplied but could not be retrieved (the page
  requires a logged-in browser), so it did not inform this design.

## 1. Where the two designs agree

Both designs land on the same skeleton, and the implementation keeps it as-is:

| Decision | Doc A | Doc B |
|---|---|---|
| Canonical store is plain Markdown + YAML frontmatter in git | `knowledge/` OKF v0.2 bundle | `.agent-kb/concepts/` OKF-style nodes |
| One atomic claim per file | Zettelkasten granularity | "single atomic concept" |
| Retrieval is index-first and progressive, not injected | ~25-line pointer, hierarchical indexes, search CLI | <150-token bootstrapper, `kb_search` then `kb_fetch` |
| Writes go through a tool, never raw edits | `kn add` / MCP | `kb_propose` |
| Every claim is a small state machine verified by a *different* agent | `status: draft` until another family verifies | `unverified -> corroborated/disputed`, self-verification rejected |
| Cross-review is a first-class record | `Review` task, `reviewer_must_differ_from` | review packet with git range |
| Hermes syncs over git | git transport, cron pull | git auto-sync fallback |
| Deterministic lint, index is generated | pre-commit schema + retired-terms lint | `agent-kb lint`, `sync-index` |

## 2. Where they differ, and what this implementation picks

### 2.1 Directory and schema: OKF-native fields (Doc A) + code anchors (Doc B)

Doc B invents its own frontmatter (`id`, `trust_state`, `proposed_by`,
`verified_by`, `verified_commit`). Doc A uses OKF v0.2's own vocabulary
(`generated`, `verified`, `status`, `stale_after`, `sources`). **We use the OKF
vocabulary**, because OKF already defines exactly these concepts and an
OKF-aware tool (Obsidian exporter, `okf-mcp`, future agents) can read the bundle
without translation. We add Doc B's best idea as OKF extension keys, which the
spec permits:

- `resources:` code globs the claim is about. This powers path-aware search and
  code-anchored staleness.
- `generated.commit` / `verified[].commit`: the git `HEAD` at write/verify time.
- `verified[].result` (`confirmed` | `disputed` | `approved` | `changes_requested`)
  and `verified[].evidence`.
- `supersedes:` for Decision chains, `review:` for review packets.

Two of Doc B's fields are dropped on purpose:

- **No `id` field.** OKF says a concept's ID is its path. A stored ID can drift
  from the filename; a path can't.
- **No stored `trust_state`.** Trust is *derived* on every read from the
  append-only `verified` list, `stale_after`, `supersedes` links and git
  history. A stored state field is a second source of truth that drifts, which
  is the exact failure mode both documents are trying to prevent. OKF `status`
  (`draft|stable|deprecated`) is kept for OKF compatibility, written only by the
  tool, and checked by lint.

The directory is `knowledge/` (Doc A), not `.agent-kb/` (Doc B): visible
directories show up in PR review, in Obsidian (which ignores dot-folders), and
in GitHub's file browser. Typed subdirectories (`facts/`, `learnings/`,
`decisions/`, `conventions/`, `reviews/`) make per-directory indexes and
per-type lifecycle rules trivial.

### 2.2 Trust model: family-level guardrail, human override, derived states

Doc B rejects `verifier == proposer` at the agent level. Doc A requires a
different *agent family*. **We implement Doc A's rule**, because the point of
cross-checking is model independence: Cursor running Claude corroborating Claude
Code is not independent. Family is resolved from the actor string
`<producer>/<model>`: a recognised model prefix wins (`cursor/claude-sonnet-4.5`
is `anthropic`), otherwise the producer default applies (`codex` is `openai`).
The mapping lives in `kb.yaml`.

Derived states, highest precedence first:

| State | Rule |
|---|---|
| `deprecated` | `status: deprecated`, or another node `supersedes` it |
| `disputed` | a cross-family `disputed` event after the last human event |
| `reviewed` | last human event confirmed it, no dispute since |
| `corroborated` | at least one cross-family `confirmed` event |
| `unverified` | everything else |

`stale` is an orthogonal flag, set when `stale_after` has passed **or** when any
file matched by `resources` changed in git since the anchoring commit (the last
confirming commit, else the generation commit). Disputes are resolved only by a
human (`kb resolve --as human:<id>`), per Doc A.

### 2.3 Index: in-process BM25 instead of a SQLite FTS5 cache

Both documents specify a derived, gitignored SQLite FTS5 database. **We don't
ship one.** At the scale both documents target (hundreds of files, with
embeddings deferred past ~200), parsing the whole bundle and ranking with BM25
in-process takes milliseconds. A cache adds the exact bug class the research
warns about (an index that disagrees with disk), plus a WAL file, an
invalidation hook, and a 10 MB SQLite dependency on every host including
Hermes. The search layer is one function behind a small interface, so a
persistent FTS or embeddings index can be added later without changing callers.

### 2.4 Write path: trust state is the inbox

Doc A stages proposals in `.inbox/` and promotes them with a nightly
consolidator. Doc B writes directly as `unverified`. **We follow Doc B.** With
the claims ledger from Doc A's addendum, `unverified` already *is* the staging
area: unverified claims are labelled as hypotheses and ranked below verified
ones. A second staging layer would hide new claims from the agents who are
supposed to verify them. Duplicate control moves to write time: `kb add`
rejects near-duplicates (title/description Jaccard ≥ 0.85) and reports related
claims in the 0.5–0.85 band so the agent can verify or dispute them instead.

Rewrite corruption is bounded by policy, as in Doc A: agents never rewrite a
file. The tool appends to `verified`, sets `status`/`stale_after`, and appends
review verdicts. Content changes happen by superseding. `decisions/` and
`learnings/` are immutable, and `kb lint --staged` fails a commit that edits
their body.

### 2.5 CLI first, MCP second

Doc B makes MCP primary. Doc A's addendum makes a CLI primary with an MCP
wrapper for clients that cannot shell out. **We follow the addendum.** Claude
Code, Codex, Cursor and Hermes can all run shell commands, and a CLI costs zero
context per session, while every MCP tool schema is paid for in every session.
The same binary serves `kb mcp` (stdio, five tools) for Hermes gateways and IDEs
that prefer it. There is no HTTP listener: Doc B's Tailscale endpoint is a
network-exposed write path (the claude-mem port-37777 class of problem). Git
transport covers Hermes.

### 2.6 Capture: hooks where they exist, honest gaps elsewhere

- **Claude Code**: `SessionStart` prints a one-to-three-line brief (reviews
  waiting for you, disputed claims) into context. `PreCompact` and `SessionEnd`
  spawn a detached `kb extract`, which condenses the transcript and runs a cheap
  model (configurable, default `claude -p --model haiku`) to propose at most
  five claims as `unverified`. A per-session byte offset prevents re-extracting
  after compaction. A recursion guard (`KB_EXTRACTING`) stops the extractor's
  own session from triggering the hook.
- **Codex**: no lifecycle hooks are documented, so `kb extract --scan codex`
  walks `~/.codex/sessions/**/rollout-*.jsonl` filtered to this repo's `cwd`
  (run it nightly), plus the pointer instruction to `kb add` explicitly.
- **Hermes**: the skill instructs it to use `kb`. `kb sync` on cron does pull,
  re-index, commit `knowledge/` only, and push.
- **Cursor / Antigravity / GrokBot**: read-mostly via the `AGENTS.md` pointer.

### 2.7 Reviews as Markdown, not JSON

Doc B stores review packets as JSON. **We store them as OKF Markdown**
(`type: Review`) so they are human-readable, diffable, linted and searchable
like everything else. The packet holds summary, assumptions, `base..head`,
touched files (computed from git) and `reviewer_must_differ_from`. Following
Doc A's independence rule, the packet contains claims and the diff only, not the
author's reasoning. `kb review show` adds the diffstat and the corroborated
invariants whose `resources` match the touched files. Verdicts are appended
events, and the reviewer's family must differ from the author's. `kb review
revise` records a new `head` after fixes and reopens the review.

### 2.8 Things deliberately left out (for now)

- `.inbox/` and a nightly LLM consolidator (see 2.4). `kb maintain` (index +
  lint) is deterministic and cron-safe.
- Embeddings, Notion/Obsidian/NotebookLM publishing, Tailscale HTTP MCP.
- A `local`/`user` scope inside the bundle. Per Doc A's addendum, user
  preferences stay in each agent's private memory; machine-local state lives in
  the gitignored `knowledge/.kb/`.
- A "digest" injected every turn (Doc A's original draft, replaced by its own
  addendum with the pointer + search model).

## 3. Resulting architecture

```
repo/
  AGENTS.md                 <!-- kb:begin --> ~20-line pointer <!-- kb:end -->  (Codex, Cursor, Antigravity)
  CLAUDE.md                 @knowledge/POINTER.md                           (Claude Code)
  .claude/settings.json     hooks: SessionStart / PreCompact / SessionEnd -> kb hook claude
  .claude/skills/kb/        progressive-disclosure skill (full command reference)
  knowledge/
    kb.yaml                 config: types, TTLs, families, retired terms, extractor commands
    POINTER.md              generated pointer (same text everywhere)
    README.md               for humans
    index.md                generated root index (<= 60 lines)
    log.md                  append-only audit log (merge=union)
    facts/ learnings/ decisions/ conventions/ reviews/
      index.md              generated per-directory index (<= 200 lines)
      <slug>.md             one OKF node per claim
    .kb/                    gitignored: lock file, extraction offsets, logs
```

```
              read (no tool schema cost)                write (tool-stamped, append-only)
 agent ──> pointer ──> kb search --file ──> kb show      kb add ─┐
                         │  BM25 + path boost            kb verify ─┼─> flock ─> atomic write ─> log.md
                         │  derived trust + git staleness kb review ─┘
                         ▼
              knowledge/*.md  <── git ──>  Hermes host (kb sync on cron)
```

Single static Go binary `kb` (only dependency: `gopkg.in/yaml.v3`):

| Command | Purpose |
|---|---|
| `kb init` | create bundle, config, pointer, `.gitignore`/`.gitattributes` entries |
| `kb install claude\|codex\|cursor\|antigravity\|hermes\|git-hooks` | write pointer blocks, skill, hooks, git hooks; print MCP snippets |
| `kb search [query] [--file p]...` | compact ranked headers with trust badges (≈ 15–25 tokens per hit) |
| `kb show <id>...` | condensed node(s): trust trail, resources, body |
| `kb add <type> <title> -d -b -r -t -s` | propose a claim (`unverified`), duplicate check, secret redaction |
| `kb verify <id> --confirm\|--dispute -e` | cross-family verification, commit-stamped |
| `kb resolve <id> --keep\|--deprecate --as human:x` | human resolution of disputes |
| `kb deprecate <id>` | retract (human or the generating family) |
| `kb review submit\|list\|show\|verdict\|revise\|close` | cross-review loop |
| `kb status [--brief]` | counts, disputes, stale, reviews waiting |
| `kb index` / `kb lint [--staged] [--fix]` / `kb maintain` | deterministic upkeep |
| `kb extract` / `kb hook claude` | capture from transcripts |
| `kb sync` | git pull/rebase, re-index, commit `knowledge/`, push |
| `kb mcp [--as actor]` | stdio MCP server: `kb_search`, `kb_show`, `kb_add`, `kb_verify`, `kb_review` |

## 4. Phasing (revised)

1. **Done in this repository:** format, CLI, trust state machine, reviews, lint,
   index, pointer/skill/hook installers, MCP server, Claude Code capture, Codex
   rollout importer, git sync for Hermes.
2. **Next:** run it on a real repo for two weeks; measure per Doc A's metrics
   (`kb status --json`: unverified age, stale count, disputes, claims per actor).
3. **Conditional:** embeddings behind `kb search` past ~200 nodes; Notion or
   Obsidian as viewers; HTTP MCP only behind authentication.

## 5. Unverified integration details

Things this implementation relies on that should be checked against current
vendor docs before depending on them:

- Claude Code hook input fields (`transcript_path`, `session_id`,
  `hook_event_name`) and that `SessionStart` stdout is added to context.
- Claude Code exports `CLAUDECODE=1` to tool shells (used for actor detection).
- Codex rollout location `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` and its
  `session_meta.cwd` field; the env var Codex sets in tool shells (we recommend
  setting `KB_ACTOR=codex` explicitly via `shell_environment_policy`).
- Codex skills directory (`.codex/skills/`) and Hermes skills/MCP config paths.
