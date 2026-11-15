# AgentKB architecture

This document reviews the input designs, records the architecture that was
built, and explains every deviation. Revision 2 (2026-11-08) folds in the
user's clarified priorities; the decisions it changed are marked. Revision 3
(2026-11-15) adds §2.11: rule scope per delivery surface, baseline checkout
selection and explicit unknown code freshness.

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
  `--all` widen it. This is the *discovery* scope. Rules use a narrower
  *applicable* scope (§2.11).
- **Staleness across projects:** a gitignored registry (`.kb/registry.json`)
  records local checkouts as `kb` runs in them, so an entry from another
  project is still checked against its own repo when that repo is on this
  machine. Which checkout is used, and what happens when none can be, is in
  §2.11.
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

  Output is capped at 40 rules, one line each, and push surfaces also fit
  their whole payload to a size budget (§2.12). Every surface selects rules
  through one function with the applicable-rule scope of §2.11.
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
  - Claude Code's typed memory files are imported without a model and keep
    their identity; all imports are source-only (§2.13).
- **Rule-file import:** `kb rules import` splits rule files written for one
  agent into atomic, agent-neutral rules. Files are read the way their own
  agent loads them, and a path-scoped rule stays conditional (§2.14).
  - User-level files become global rules; a repo's `CLAUDE.md`, `GEMINI.md`,
    `.claude/rules`, `.cursor/rules`, `.agents/rules`, `.windsurfrules` and
    Copilot instructions become project rules.
  - `AGENTS.md` is skipped (Codex, Cursor and Antigravity read it natively),
    as are kb's own managed blocks (no feedback loop).
  - Imported rules skip the doc-overlap check, because they are delivered
    rather than searched.
- **Ownership marker:** user-level managed blocks carry a marker naming the
  hub that wrote them, and refreshes only touch blocks they own.
- **Agent-driven seeding:** an agent that already knows things about some
  projects writes them itself with `kb add --from` (a JSON array or JSON
  lines; per-item `project` or `global`). That's the cheap path inside a
  session: no extra model call, and the agent judges what is durable. The
  importer can also be limited with `kb extract --scan memories -p <id>`; a
  filtered run never records the file as imported, so a later full run still
  takes the rest.
- **Project-name guard:** writing to a project that doesn't exist (other than
  the current repo's) needs `--new-project`; otherwise kb refuses with a
  did-you-mean suggestion, so seeding into named projects can't silently
  create stray ones.

### 2.11 Reviews

Reviews are OKF Markdown packets (`type: Review`) bound to `base..head`. Files
come from git, with knowledge anchored to the touched files attached. Verdicts
are events scoped to the head; `kb review revise` moves the head and reopens
the review. Rev 2 allows same-family reviews but labels them (a fresh session
is a useful reviewer), keeping cross-family review as the stronger option.

### 2.11 Rule scope, baselines and code freshness *(rev 3, issue #1 items 1 and 2A)*

**Two scopes.** §2.4's all-project default is right for discovery: Hermes
must be able to find anything. It is wrong for rules, because a rule is an
instruction and one project's rule must not steer a session elsewhere. Rules
therefore have their own selection (`Store.Rules`), used by every surface:

| Surface | No recognized project | Recognized project |
|---|---|---|
| `SessionStart` rules, MCP `instructions`, plain `kb rules` | global rules with no `projects:` restriction | the project's rules + global rules that apply to it |
| User-level Codex/Antigravity blocks | global rules with no `projects:` restriction | the same (the files are user-wide) |
| `kb rules -p <project>` | that project's rules + global rules that apply to it | the same |
| `kb rules --all` | every project's rules, labelled as inspection | the same |
| search / status / review lists | every project (§2.4, unchanged) | project + applicable global (unchanged) |

- A global rule restricted with `--for` is delivered only where it applies.
  It used to leak into the user-level blocks.
- `kb status` mixes the scopes on purpose and says so: entry counts follow
  `scope`, `rules_delivered` follows `rules_scope`. `rules_delivered` still
  counts applicable rules before any cap; `rules_omitted_by_cap` is what
  session start leaves out (§2.12).
- The health brief injected at session start follows the session: project
  plus the global entries that apply to it, or outside a project only global
  entries with no `projects:` restriction (not the whole global bundle,
  which discovery would select). It is at most three lines,
  in priority order (reviews, disputes, proposed rules, stale entries, one
  aggregate unknown-freshness line). Interactive `kb status` keeps its
  default.

**Code freshness is separate from trust and from the deadline.** Derived
trust gains a live `code_freshness`: `unchanged`, `changed`, `unknown` or
`not_applicable`. Before, a comparison that could not be made (anchor commit
missing, no checkout) left `stale: false`, which read as fresh.

- `stale` keeps its meaning (code changed, or past `stale_after`) and its
  JSON name and type. `unknown` alone does not set it. Both can hold at once.
- Ranking: one multiplier on the existing score: 0.80 when stale, 0.90 when
  unknown, else 1.00; never both. These are initial values to revise against
  a benchmark.
- Unknown entries stay searchable and eligible for `--trusted`; nothing is
  demoted or re-verified automatically.
- It is live output only. Indexes stay deterministic (static trust).

**Baseline selection.** The registry now records each checkout's repository
identity (normalized remote) and git common directory. These and the
preferences live in `.kb/checkouts.json`; `.kb/registry.json` keeps its
original project-to-paths format, so an older binary on the same machine
keeps working and cannot drop a preference when it rewrites the file. Paths
recorded by older versions are inspected once. Updates reread, change and
write both files under a dedicated cross-process lock (`.kb/registry.lock`);
`checkout set`/`clear` fail when the change cannot be saved, while automatic
registration stays best effort. For the repository an entry belongs to, the baseline is,
in order:

1. an explicit `--checkout` (and `--ref` instead of HEAD);
2. the current checkout, when it is that repository;
3. the checkout set with `kb project checkout set` (local, validated);
4. the primary worktree from git's metadata, when the repository has a single
   clone here.

Anything else is `unknown` with a reason. Registration order never decides.
The selected repository, path, commit and rule are exposed in `kb show` and
`--json`.

**Owning repository (legacy entries).** Entries don't record their repository
yet (structured anchors are stage 2B). In a multi-repository project it is
inferred from the anchor commit when exactly one repository contains it and
every repository of the project could be looked in. Containment is a hint,
not proof: forks share commits, a missing object proves nothing, a
repository that is not checked out here may contain the commit too, and a
commit stamped by a memory or rule import (a source id `memory-*`/`rules-*`
whose anchor is the import event) is not used. Several matches, or a match
with candidates that could not be checked, are settled only by `--checkout`
or the repository kb runs in; otherwise the entry is unknown. A write from outside a multi-repository
project records no commit.

**Not done here:** structured multi-repository anchors and branch
applicability (2B), source-only import anchors and deadlines (4A). Until 4A,
an import into a single-repository project still records the checkout's HEAD.
Global entries are `not_applicable`: nothing ties them to one repository.

### 2.12 Delivery budgets *(issue #3 item 1)*

A count cap does not bound size. Claude Code caps a hook's stdout at 10,000
characters; past that, Claude gets a file path and a 2,000-character preview
and is not asked to read the file. Forty rules of full length came to about
12,000 characters, so most of them would have gone missing without an error.

- Each push surface declares a budget for its **complete payload**: rule
  lines, ids, headings, the omission notice and the health brief.
  SessionStart: 9,000. MCP `instructions`: 9,000, a context-cost choice of
  our own, since no host limit is documented for that field.
- One pure planner (`planDelivery`) fits the payload. Whole lines only. When
  it is too big, brief lines go first (replaced by a one-line notice), then
  rules from the end: global before project, as the count cap does. The
  40-rule cap stays as a second bound.
- Sizes are counted in **UTF-16 code units**, which is how a JavaScript host
  measures string length and is never less than the number of characters.
  Issue #3 proposed 9,000 UTF-8 bytes; that is safe too, but it would cut
  Chinese text to a third of what the documented limit allows.
- `kb status` runs the same planner with the same inputs as the hook and
  reports it under `rules_delivery` (surface, scope, unit, budget, size,
  applicable, rendered, omitted by count and by size, brief lines kept and
  left out). `rules_delivered` is unchanged: applicable rules before any
  cap. `rules_omitted_by_cap` is now the rules session start leaves out for
  either reason.
- `kb rules --all-rules` lists what a cap left out. Plain `kb rules` keeps
  the 40-rule cap and no size budget: it is pulled on request.
- Not budgeted here: the user-level Codex/Antigravity blocks, which are
  files, not hook output, and keep the count cap only.

### 2.13 Source-only ingestion and the Claude memory adapter *(issue #3 item 2, issue #1 stage 4A for imports)*

**Source-only writes.** A memory or rule file that is imported was not
checked against the code at import time. `Store.Add` and `Store.Update` take
an explicit `SourceOnly` origin instead of inferring it from the evidence
text:

- No commit is recorded, neither on the generation event nor on the
  confirmation the evidence creates. The HEAD of whatever checkout the import
  ran near says nothing about the claim. An entry with `resources` and no
  anchor has unknown code freshness (§2.11), not a pass.
- Evidence still makes the entry `verified`: source-backed trust is unchanged
  (§2.10). Trust, the deadline and code freshness stay independent.
- For types that expire, `stale_after` is `min(source date, import time) +
  TTL`, never later than an import-time deadline. `stale_basis` says what the
  date was: `source_updated_at` (a revision date the source records about
  itself), `source_mtime` (the file's modification time) or
  `undated_review_schedule` (no usable date, or a future one: import time
  plus at most seven days). None of them is an observation date. An old note
  is therefore due at once, and its stale reason names the basis.
- Refreshing an entry from a changed source never moves its deadline later.
  A real check (`kb verify --confirm`, an ordinary `kb update`) records a
  commit, restarts the deadline from now and drops the basis.
- `sources[]` keeps `last_modified` (the source's own date), `ingested_at`
  and, when the source names one, `origin_session` (lineage only: it does not
  say the session was fully captured).

This covers imports. Transcript extraction still records the session
checkout's HEAD; that part of issue #1 stage 4A is not done here.

**Claude Code auto memory.** Each memory is one file with YAML frontmatter
(`name`, `description`, `type`, often `modified` and `originSessionId`) next
to a `MEMORY.md` index. That layout is what Claude Code writes today, not a
documented contract, so the adapter takes only what it recognises and sends
the rest to the model-based import, visibly:

| File | Outcome |
|---|---|
| `type: feedback` | convention in the directory's project, in the source's wording; never promoted to global |
| `type: project`, `type: reference` | fact with the type's normal expiry; a memory type alone never makes a decision |
| `type: user` | skipped and counted: notes about the person stay private |
| `MEMORY.md` with only links and headings | skipped: it restates the topic files |
| `MEMORY.md` with other lines | model fallback on those lines only (its links are left out) |
| no frontmatter, unknown type, several headings, or over 6,000 bytes | model fallback, reported with the reason |

**Identity.** A versioned, gitignored ledger (`.kb/extract/import-ledger.json`)
records for each source file its directory, content hash, the entry it became
and the revision the import last wrote.

- Unchanged file: nothing happens.
- Edited file: the same entry gets a new revision through a conditional
  `Store.Update`. If the entry was edited since the last import, that is a
  conflict: nothing is overwritten, and it is reported on every scan until it
  is merged by hand and settled with `--accept-conflicts`.
- Renamed file: recognised when exactly one new file carries a vanished
  source's content (or name). Several identical candidates are reported as
  ambiguous; nothing is imported or flagged for them.
- Memory whose type changes to one that maps to another knowledge type: a
  conflict on every scan, decided on the entry's actual type. An update
  cannot change an entry's type, so the source is never applied to it, and
  accepting the conflict does not lift that. Deprecating the entry frees the
  memory to import as a new entry.
- Tracked file that stops being a single typed note (several sections, too
  long, now `user` or empty): also a conflict. The model path only adds
  entries, so it must not take a file that already owns one.
- Deleted file: the entry is kept and reported for review until it is
  confirmed again (a check recorded after the deletion was found), rewritten
  or deprecated. Deletions are reported only when the whole
  directory was read: an unreadable file or directory is not a deletion.
- A directory with no project to attach to is reported and skipped.

Importing a source that overlaps a transcript of the same session
automatically stays out until session coverage exists (issue #1 stage 3B).

### 2.14 Conditional rules and rule-file fidelity *(issue #3 item 6A)*

**Applicability is not anchoring.** `resources` says which code a claim
depends on, for search and staleness. It never limited when a rule is
delivered, so copying a path-scoped rule's patterns into `resources` would
have made it an always-on rule. A rule that applies only to some files now
carries `applies_to`:

```yaml
applies_to:
  paths: ["src/api/**/*.ts"]
  base: packages/web        # directory the patterns are relative to
  origin: claude-rules      # whose pattern syntax
```

- A conditional rule is trusted and in scope but is left out of every
  always-on surface: `kb rules`, SessionStart, MCP `instructions`, the
  user-level blocks, and `rules_delivered`. Those surfaces carry one line
  saying how many such rules exist. `kb status` counts them separately
  (`rules_conditional`).
- It surfaces for a matching file: `kb rules -f <path>` and `kb search -f
  <path>` (which the pointer already tells agents to run before touching
  unfamiliar code). Asked about several files at once, each rule names the
  files it governs, so a rule for one package is never presented as a rule
  for another. `kb search -f <path> --json` lists the same rules (`kind:
  "rule"`) with `applies_to` and, in `rule_for`, the files asked about that
  each one governs. `kb rules --all-rules` lists them with their condition.
  Nothing pushes them yet; a file-access hook is issue #3 item 4.
- Only rules can be conditional (`kb add rule ... --when <glob>`). `--when`
  repeats or takes a comma-separated list; a comma inside `{a,b}` belongs to
  its pattern. With `--from` it limits every seeded rule, and an item that
  is not a rule is refused instead of being added without the condition.
- Patterns follow Claude Code's documented rule syntax: `**` crosses
  directories; a bare name matches at any depth; a leading `/` anchors to the
  base; `{a,b}` expands (bounded at 1,000); `[...]` and `\` escapes work,
  and a bracket expression, negated or not, stays within one path segment
  (one whose range takes in `/`, such as `[.-0]`, is refused). A
  pattern that cannot be matched exactly is an error and matches nothing. It
  is never widened.

**Reading rule files as their agent does.** `kb rules import` now:

| Found | Outcome |
|---|---|
| Claude rule with `paths:` | imported as conditional, base = the directory holding its `.claude` |
| `CLAUDE.md` or rules in a subdirectory | conditional on that directory (they load only for files there) |
| frontmatter that does not parse | not imported: Claude Code would load it unconditionally, kb must not guess |
| a pattern kb cannot match exactly | not imported |
| Cursor rule that is not `alwaysApply: true`, other agents' `globs`/`trigger` | not imported: kb cannot reproduce that condition |
| `CLAUDE.local.md` | not imported: personal, uncommitted instructions stay private |
| an `@path` import of `CLAUDE.local.md`, or of a file git ignores | not followed, for the same reason |
| a link or `@path` import leaving the repository (or `~/.claude` for user files) | not followed |
| a repository instruction file that is itself a link to somewhere outside it (a nested `CLAUDE.md` included) | not imported |
| `@AGENTS.md` | not followed: other agents read it natively |
| kb's own blocks and pointer | never re-imported |

Rules directories are walked recursively, each directory once whatever leads
to it (a link back to the directory or an ancestor ends there). `@path`
imports are followed up to
four hops, once each (cycles end), only to existing files, and never inside
code spans or fences. Block-level HTML comments are dropped, except inside
code blocks. A fenced block ends only at a fence of the same character that
is at least as long, so a block that shows Markdown with its own fences
stays one block. Everything left out is listed as a `note:` line.

**The condition is part of what was imported.** A file's condition lives in
frontmatter (or in where the file is), not in the text sent to the model, so
the import's change detection covers both. When only the condition changes,
the rules the file gave earlier (found by their `sources`) take the new
condition as a new revision; a rule that was in force stays in force.
Two cases never widen a rule. A file that was emptied states no condition,
so the rules it gave keep theirs. A file whose condition becomes one kb
cannot reproduce (an always-on Cursor rule given `globs`, a pattern that
cannot be matched exactly) is no longer imported, and the rules it gave
earlier are retired, with the reason, instead of staying in force more
widely than the file now says. Fixing the file and importing again adds
them back.

**A rule two files state belongs to both.** When a file adds nothing because
an existing rule already says it, the import remembers which rule it relied
on. If that rule is later narrowed or retired for its own file's sake, the
files that relied on it are read again (in the same run), so the rule stays
in force wherever another file still states it.

**A rule repeats another only where that one is already delivered.** The
same instruction in `packages/web/CLAUDE.md` and `packages/api/CLAUDE.md` is
two rules, one per directory. A proposal is a duplicate when an existing rule
with the same text is unconditional, or has the same base and syntax and all
of the proposal's patterns. Conditions are never merged or widened. The
import prompt lists existing rules with their condition and says which files
the file's own rules are for.

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
| `kb project checkout [list / set / clear]` | this machine's checkouts; choose the baseline code is compared with |
| `kb install claude / codex / cursor / antigravity / hermes / git-hooks [--global / --repo]` | pointer, skill, hooks (user-wide with a hub) |
| `kb search [query] [-f path] [-p project] [--all]` | compact ranked headers with trust badges |
| `kb show <id>` | entry with trust trail, revisions, resources, fenced body |
| `kb add <type> <title> -d ... [-e evidence] [--global [--for ...]]` | record knowledge (`rule` = convention) |
| `kb rules [-p project / --all] [-f path] [import]` | the rules that apply here (§2.11), or only to a file (§2.14); share rule files written for one agent |
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
