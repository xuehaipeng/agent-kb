# AgentKB architecture

This document reviews the input designs, records the architecture that was
built, and explains every deviation. Revision 2 (2026-11-08) folds in the
user's clarified priorities; the decisions it changed are marked.

Inputs:

- **Doc A**: *Build a git-backed knowledge bundle agents push into*, plus its
  addendum (Claude Code and Codex primary, Hermes remote, thin retrieval).
- **Doc B**: *Technical Design & Implementation Plan: AgentKB* (single binary,
  `kb_search`/`kb_fetch`/`kb_propose`/`kb_verify`/`kb_review`, code-anchored
  staleness, review packets).
- **Doc C**: *Cross-Agent Knowledge and Review System* (the ChatGPT plan):
  evidence model, exact-artifact reviews, publication guarantees, evaluation.
- **User priorities (rev 2):**
  1. Knowledge sharing is the main goal; cross-checking is one function among others.
  2. A confident agent's knowledge should count without a second agent; keeping entries current matters more than gating them.
  3. Knowledge must be grouped by project/repo across many repositories (personal and organisation repos).

## 1. What all inputs agree on (kept as-is)

| Decision | Where it comes from |
|---|---|
| Canonical store is Markdown + YAML frontmatter in git | A, B, C |
| One small claim per file | A (Zettelkasten), B (atomic concepts) |
| Retrieval is index-first and on demand, not injected | A addendum, B, C §7 |
| Writes go through a tool, never raw edits | A, B, C §9 |
| `generated` and `verified` are separate events | A (OKF), C §3.3 |
| Reviews bind to exact commits; a new head invalidates old verdicts | B, C §8.3 |
| Changed dependencies trigger revalidation, not falsification | B (git staleness), C §9.2 |
| Hermes reaches the knowledge over git, not a shared home dir | A, C §10 |
| Deterministic lint and generated indexes | A, B |

## 2. Decisions and deviations

### 2.1 Schema: OKF vocabulary + code anchors

OKF v0.2 fields (`type`, `title`, `description`, `generated`, `verified`,
`status`, `stale_after`, `sources`) plus extension keys: `resources` (code
globs), `revision`/`updated` (rev 2), `projects` (global entries, rev 2),
`supersedes`, `review`, and per-event `result`, `rev`, `commit`, `evidence`.

Dropped from Doc B: a stored `id` (OKF IDs are paths) and a stored
`trust_state` (a second source of truth that drifts). OKF `status` is kept
for compatibility, written only by the tool, and checked by lint.

### 2.2 Trust model *(changed in rev 2)*

Rev 1 required a different model family before an entry counted
(`unverified` until cross-checked; same-family verification rejected). The
user rejected this as too strict: most simple-to-medium tasks are done by one
agent, and gating knowledge behind a second agent starves sharing. Rev 2:

| State | Rule (computed from events on the **current revision**) |
|---|---|
| `deprecated` | `status: deprecated`, or superseded |
| `disputed` | the latest event is a dispute |
| `reviewed` | a human confirmed it after the latest dispute |
| `cross-checked` | a family other than the content author's confirmed it |
| `verified` | the author's family confirmed it with evidence, including at write time (`kb add -e`) |
| `unverified` | no evidence yet: a lead |

`stale` stays an orthogonal flag (past `stale_after`, or `resources` changed
in git since the last confirmation). Nothing stores the state.

The latest event wins, so agents can settle a dispute themselves by
re-checking or by fixing the entry; `kb resolve` remains for explicit human
calls. Following Doc C ("repetition is not corroboration"; "different brands
are not statistical independence"), cross-family confirmation is shown as a
stronger signal and ranked higher, not counted as proof.

### 2.3 Evolving entries *(new in rev 2)*

`kb update` rewrites an entry's content as a new revision: `revision: N`,
`updated: {by, at, commit, evidence: <reason>}`. Each verification event
carries the revision it checked (Doc C's `record_revision`), so the new
content starts fresh, the old checks stay visible as history, and git keeps
every prior version (hub autocommit gives one commit per change). Any agent
may update or deprecate with a reason.

Rewrite drift (Doc A's ~25% corruption after 20 LLM edits) is bounded by
keeping entries small, requiring a reason per revision, keeping git history,
and showing which revision each check covered. Decisions remain immutable and
change by superseding, because the sequence of decisions is itself knowledge.
Learnings are mutable in rev 2 (they were immutable in rev 1).

### 2.4 Multi-project hub *(new in rev 2)*

Rev 1 put a `knowledge/` bundle inside each repo. For a user working across
many repos, including organisation repos shared with teammates, that:
1. needs the team's buy-in to commit agent files into shared repos;
2. leaves no home for knowledge that spans projects;
3. makes Hermes clone every code repo to read knowledge.

Rev 2 makes a hub the default:

- `~/.kb` (or `$KB_HOME`) is one git repo, ideally with a private remote. It
  holds `global/` and `projects/<id>/`, each with the usual typed directories.
- **Project detection:** the current repo's `origin` URL is normalised
  (`git@github.com:Org/Repo.git` becomes `github.com/org/repo`) and looked up
  in each `projects/<id>/project.yaml` `repos:` list. Otherwise the id is the
  repo name, or `owner-name` if another repo already claimed that name.
  `kb project link <id>` maps forks, renames and multi-repo projects (backend
  + frontend).
- **Scope:** default search, status and review lists cover the current
  project plus global entries whose `projects:` list is empty or includes it.
  Outside any repo (e.g. on Hermes) the default is every project. `-p` and
  `--all` widen it.
- **Staleness across projects:** a gitignored registry (`.kb/registry.json`)
  records local checkouts as `kb` runs in them, so an entry from another
  project is still checked against its own repo when that repo is on this
  machine.
- **Sharing:** every write autocommits to the hub. `kb sync` does pull,
  rebase, reindex and push. Claude Code hooks pull at session start and push
  after extraction at session end, both in the background.
- **Installs are user-wide:** `~/.claude/CLAUDE.md` imports
  `~/.kb/POINTER.md`; `~/.codex/AGENTS.md` carries the pointer; skills live in
  `~/.claude/skills` and `~/.codex/skills`. Nothing is written into code
  repos.
- **Repo mode stays:** a repo with `knowledge/kb.yaml` uses its own bundle
  (this repository does), and `kb hub import` moves such a bundle into the
  hub.

### 2.5 Index: in-process BM25, no SQLite cache

Docs A and B proposed a gitignored SQLite FTS5 index. At hundreds of entries
per project, parsing everything and ranking with BM25 in-process takes
milliseconds and can't disagree with disk. Search sits behind one function,
so a persistent or embedding index can be added later. Han, Kana and Hangul
text are tokenised as unigrams and bigrams.

### 2.6 Write path: no inbox

Entries land directly in their typed directory, verified or unverified
depending on evidence (rev 2). A Doc A-style `.inbox/` plus consolidator would
hide new knowledge from the agents who need it. Duplicates are caught at write
time (Jaccard ≥ 0.85 within the project), and related entries (0.4–0.85) are
pointed out so agents update them instead of adding variants.

### 2.7 CLI first, MCP second

Shell commands cost no context until used, and every MCP tool schema is paid
for in every session. `kb mcp` serves six tools over stdio for clients that
prefer tools. There is no HTTP listener, since that would be an
unauthenticated write path.

### 2.8 Capture

- **Claude Code:**
  - `SessionStart` prints at most a few lines (reviews waiting, disputes, stale entries) and pulls the hub.
  - `PreCompact`/`SessionEnd` run `kb extract` detached, then sync.
  - Per-transcript offsets and a recursion guard make it safe.
- **Codex:** no hooks, so `kb extract --scan codex` reads `~/.codex/sessions` and routes each session to the project of its `cwd`.
- **Hermes:** the skill, `kb add -p <project>`, and `kb sync` on cron.

Per Doc C, hooks are not the only capture path: the pointer tells agents to
record durable findings at the moment they're established.

### 2.9 The repo's own docs are the source of truth *(new in rev 2)*

When a project already has detailed, current documentation (README, `docs/`,
ADRs, `AGENTS.md`), copying it into the knowledge base adds a second source of
truth that drifts as soon as the doc changes. It also costs tokens, since
agents can read the doc directly. The first dogfooding round showed the risk
is real: 7 of this repository's 8 entries restated `docs/ARCHITECTURE.md` or
`AGENTS.md`, because nothing told the agent the docs existed. Now:

- **Search reads the docs live.** `kb search` also returns matching sections
  of the docs tracked in the current project's checkout, as
  `doc path:line (§ heading) - first sentence`. Nothing is copied; sections
  are split by Markdown headings, and dependency, hidden and boilerplate files
  are skipped. Agents find documented knowledge through the same command.
- **`kb add` refuses restatements.** It measures how much of the new entry's
  claim (title + description) a single doc section already states, weighting
  rare terms higher. At coverage ≥ 0.65 it refuses and names the section
  (`--force` overrides); from 0.45 it warns. Calibrated on real data: new
  knowledge scored 0.17–0.27, partly documented claims 0.41–0.56, restated
  sections 0.65–1.0.
- **Extraction is stricter.** It drops claims at ≥ 0.45, and the prompt lists
  the doc headings the model must not restate.
- **`kb lint` warns** on existing entries that restate a doc section.
- **The pointer and skill say it.** Record only what the docs don't say; if a
  doc is wrong, fix the doc.

So the knowledge base holds what docs typically don't: gotchas found while
working, cross-repo facts, environment quirks and undocumented decisions.
Coverage is lexical, so a heavy paraphrase can slip under the threshold; the
guidance covers that case.

### 2.10 Agents' memories, rules and conventions *(new in rev 2)*

Knowledge reaches the base in three ways:
1. **Recorded deliberately:** `kb add`.
2. **Extracted from transcripts:** `kb extract` on transcripts.
3. **Saved by an agent in its private memory, or written by the user for one
   agent:** Claude Code auto memory, Codex memories, Hermes
   `MEMORY.md`/`USER.md` and Antigravity knowledge items; or
   `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md`, `.cursor/rules/`. These sources
   are already distilled but stay invisible to the other agents.

Rev 2 shares all three.

- **Rules are pushed, not pulled.** A rule that has to be searched for is not
  followed, so always-on types (`convention`, alias `rule`; `always_on` in
  `kb.yaml`) are delivered:
  - Claude Code: the `SessionStart` hook prints the rules for the session's
    project plus global.
  - Codex and Antigravity: global rules live inside the managed block of
    `~/.codex/AGENTS.md` / `~/.gemini/GEMINI.md`, refreshed on every rule
    change through a change hook. Project rules come from `kb rules`.
  - MCP clients: the server's `instructions`.

  Output is capped at 40 rules, one line each.
- **Only evidence-backed rules are delivered.** A rule recorded with evidence
  (the user said so, an imported rule file, a saved memory) is applied. One an
  agent inferred on its own is listed as *proposed* in `kb rules`, `kb status`
  and the session brief until confirmed. This is the one place stricter than
  the general "one agent with evidence is enough" rule, because a bad rule
  steers every session of every agent (Doc A's finding that LLM-written
  context files lower success).
- **Memory import:** `kb extract --scan memories` (or
  `claude-memory|codex-memory|hermes-memory|antigravity-memory`).
  - It reads a file only when its content hash changed.
  - Claude Code's per-project memory is mapped to its project through a
    transcript's `cwd` or a registered checkout. Cross-project memories go to
    `global`, and the model may route single entries to a known project.
  - Entries are recorded as verified by the source agent, with the memory file
    as evidence, and pass the duplicate and doc-overlap checks.
  - Claude Code's memory is imported automatically at `SessionEnd`.
- **Rule-file import:** `kb rules import` splits rule files written for one
  agent into atomic, agent-neutral rules.
  - User-level files become global rules; a repo's `CLAUDE.md`, `GEMINI.md`,
    `.claude/rules`, `.cursor/rules`, `.agents/rules`, `.windsurfrules` and
    Copilot instructions become project rules.
  - `AGENTS.md` is skipped (Codex, Cursor and Antigravity read it natively),
    as are kb's own managed blocks (no feedback loop).
  - Imported rules skip the doc-overlap check, because they are delivered
    rather than searched.
- **Ownership marker:** user-level managed blocks carry a marker naming the
  hub that wrote them, and refreshes only touch blocks they own.

### 2.11 Reviews

Reviews are OKF Markdown packets (`type: Review`) bound to `base..head`. Files
come from git, with knowledge anchored to the touched files attached. Verdicts
are events scoped to the head; `kb review revise` moves the head and reopens
the review. Rev 2 allows same-family reviews but labels them (a fresh session
is a useful reviewer), keeping cross-family review as the stronger option.

## 3. Doc C (ChatGPT plan) review

**Adopted in rev 2** (also: §2.9, repo docs as the source of truth):
- Verification events scoped to a content revision.
- Search reports truncation ("N more") and the scope searched, so a miss isn't presented as absence.
- `kb show` fences entry content as data, not instructions.
- `kb status` shows knowledge not yet shared (uncommitted files, unpushed commits).
- Global entries can name the projects they apply to.

**Not adopted:**
- A central always-on record service with queues, leases, caches and an outbox. A git hub already gives an authority (the remote), offline pending writes (unpushed commits) and history, with nothing to operate.
- Re-platforming on Basic Memory or Notion Lore. Neither has evidence-scoped verification, code-anchored staleness or commit-bound reviews. Basic Memory is AGPL with no built-in local sync; Lore keeps the authority in Notion.

**Backlog, in priority order:**
1. An `incomplete` verdict plus coverage fields (criteria checked, tests run or unavailable) and acceptance criteria in review packets.
2. `kb review run`, which dispatches the other agent non-interactively.
3. Decision authority (who decided) kept separate from verification of the implementation.
4. A verification `method` (inspected vs reproduced) and `derived_from` lineage.
5. Branch applicability via `git merge-base --is-ancestor`.
6. Verification events as separate files, so two machines checking the same entry between syncs never conflict.
7. Doc C's four-condition evaluation (baseline / memory only / review only / both).

## 4. Resulting architecture

```
 agent ──(pointer, ~20 lines, user-wide)──> kb search ──> kb show
   │                                          │ BM25 + path boost, scope = project + global
   │                                          │ trust derived per revision, staleness from git
   └── kb add / update / verify / deprecate ──┴─> flock ─> atomic write ─> log.md + reindex
                                                                         └─> hub autocommit
 ~/.kb (git) <── kb sync ──> private remote <── kb sync ── Hermes host (~/.kb clone)
```

| Command | Purpose |
|---|---|
| `kb hub init / clone / import` | create, copy, or migrate into the multi-project hub |
| `kb project [list / link <id>]` | show/list projects; map a repo to a project |
| `kb install claude / codex / cursor / antigravity / hermes / git-hooks [--global / --repo]` | pointer, skill, hooks (user-wide with a hub) |
| `kb search [query] [-f path] [-p project] [--all]` | compact ranked headers with trust badges |
| `kb show <id>` | entry with trust trail, revisions, resources, fenced body |
| `kb add <type> <title> -d ... [-e evidence] [--global [--for ...]]` | record knowledge (`rule` = convention) |
| `kb rules [import]` | always-on rules for this scope; share rule files written for one agent |
| `kb update <id> -m <why> [...] [-e evidence]` | new revision of an entry |
| `kb verify <id> --confirm / --dispute -e ...` | record a check (any agent) |
| `kb deprecate <id> -m ...`, `kb resolve` (human) | retire or decide |
| `kb review submit / list / show / verdict / revise / close` | optional cross-review |
| `kb status [--all]`, `kb lint`, `kb index`, `kb maintain` | health and upkeep |
| `kb sync` | pull/rebase, reindex, commit, push |
| `kb extract [--scan codex/claude/memories]`, `kb hook claude` | capture from transcripts and agents' memory files |
| `kb mcp` | stdio MCP server (6 tools) |

## 5. Phasing

1. **Done:**
   - Format, CLI, trust model, evolving revisions.
   - Hub with projects, user-wide installers, sync, reviews, lint and index.
   - MCP, Claude Code capture, and the Codex scanner with project routing.
2. **Next:**
   - Run it across the real repos for two weeks.
   - Watch `kb status --all --json`: stale and unverified counts, entries per producer, updates per entry.
   - Then take the top backlog items in §3.
3. **Conditional:**
   - Embeddings once a project passes ~200 entries.
   - Obsidian as a viewer of `~/.kb`.
   - HTTP MCP only behind authentication.

## 6. Unverified integration details

These should be checked against current vendor docs and installed versions:

- **Claude Code:**
  - hook input fields (`transcript_path`, `session_id`, `hook_event_name`, `cwd`);
  - whether `SessionStart` stdout is added to context;
  - `@~/...` imports in `~/.claude/CLAUDE.md`;
  - `CLAUDECODE=1` in tool shells.
- **Codex:**
  - rollout location `~/.codex/sessions/**/rollout-*.jsonl` and `session_meta.cwd`;
  - whether `~/.codex/AGENTS.md` is read globally;
  - the skills directory;
  - `shell_environment_policy.set`.
- **Hermes:** skills directory and MCP config keys.
- **Antigravity:** reads `~/.gemini/GEMINI.md`.
