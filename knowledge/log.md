# Knowledge log

Append-only record of changes made through `kb`. Newest last. Do not edit.

- [2026-11-08T05:42:13Z] add decisions/trust-state-is-derived-on-every-read-never-stored by claude-code - Trust state is derived on every read, never stored
- [2026-11-08T05:42:13Z] add decisions/search-scans-the-bundle-in-process-with-bm25-instead by claude-code - Search scans the bundle in-process with BM25 instead of a SQLite FTS5 cache
- [2026-11-08T05:42:13Z] add decisions/the-unverified-trust-state-replaces-an-inbox-staging-area by claude-code - The unverified trust state replaces an inbox staging area
- [2026-11-08T05:42:13Z] add decisions/cli-is-the-primary-agent-interface-mcp-is-stdio-only by claude-code - CLI is the primary agent interface; MCP is stdio-only and optional
- [2026-11-08T05:42:13Z] add conventions/bundle-writes-go-through-store-methods-that-hold-store-lock by claude-code - Bundle writes go through Store methods that hold Store.Lock()
- [2026-11-08T05:42:13Z] add facts/generated-index-files-do-not-depend-on-the-clock-or-git-head by claude-code - Generated index files do not depend on the clock or git HEAD
- [2026-11-08T10:33:19Z] add learnings/resolve-symlinks-before-comparing-paths-with-git-s-toplevel by claude-code - Resolve symlinks before comparing paths with git's toplevel
- [2026-11-08T13:56:16Z] add decisions/trust-is-derived-per-revision-an-author-s-evidence-makes by claude-code - Trust is derived per revision; an author's evidence makes an entry verified (supersedes decisions/trust-state-is-derived-on-every-read-never-stored)
- [2026-11-08T13:56:16Z] add decisions/writes-land-directly-in-the-typed-directory-evidence by claude-code - Writes land directly in the typed directory; evidence decides verified vs unverified (supersedes decisions/the-unverified-trust-state-replaces-an-inbox-staging…
- [2026-11-08T13:56:17Z] add decisions/knowledge-is-grouped-by-project-in-one-git-backed-hub by claude-code - Knowledge is grouped by project in one git-backed hub
- [2026-11-08T13:56:17Z] update conventions/bundle-writes-go-through-store-methods-that-hold-store-lock by claude-code - rev 2: Update/LinkProject added; recordChange now reindexes and autocommits
- [2026-11-08T13:56:17Z] verify facts/generated-index-files-do-not-depend-on-the-clock-or-git-head by claude-code - confirmed: IndexFiles uses TrustStatic; TestIndexDeterministic passes
- [2026-11-08T13:56:17Z] verify learnings/resolve-symlinks-before-comparing-paths-with-git-s-toplevel by claude-code - confirmed: Open() still resolves via realPath; kb tests open repos through a symlink
- [2026-11-08T13:56:17Z] verify decisions/search-scans-the-bundle-in-process-with-bm25-instead by claude-code - confirmed: internal/kb/search.go Search() scans Nodes() in-process, no cache
- [2026-11-08T13:56:17Z] verify decisions/cli-is-the-primary-agent-interface-mcp-is-stdio-only by claude-code - confirmed: internal/mcp is stdio-only (Serve on stdin/stdout); no listener
- [2026-11-08T15:28:51Z] deprecate decisions/trust-is-derived-per-revision-an-author-s-evidence-makes by claude-code - restates docs/ARCHITECTURE.md §2.2 Trust model; the doc is the source of truth
- [2026-11-08T15:28:51Z] deprecate decisions/knowledge-is-grouped-by-project-in-one-git-backed-hub by claude-code - restates docs/ARCHITECTURE.md §2.4 Multi-project hub
- [2026-11-08T15:28:51Z] deprecate decisions/search-scans-the-bundle-in-process-with-bm25-instead by claude-code - restates docs/ARCHITECTURE.md §2.5 Index
- [2026-11-08T15:28:51Z] deprecate decisions/writes-land-directly-in-the-typed-directory-evidence by claude-code - restates docs/ARCHITECTURE.md §2.6 Write path
- [2026-11-08T15:28:51Z] deprecate decisions/cli-is-the-primary-agent-interface-mcp-is-stdio-only by claude-code - restates docs/ARCHITECTURE.md §2.7 and README MCP section
- [2026-11-08T15:28:51Z] deprecate conventions/bundle-writes-go-through-store-methods-that-hold-store-lock by claude-code - restates AGENTS.md (always loaded): every mutation goes through a Store method holding Store.Lock()
- [2026-11-08T15:28:51Z] deprecate facts/generated-index-files-do-not-depend-on-the-clock-or-git-head by claude-code - restates AGENTS.md (always loaded): generated outputs must be deterministic, use TrustStatic
- [2026-11-08T15:29:29Z] verify learnings/resolve-symlinks-before-comparing-paths-with-git-s-toplevel by claude-code - confirmed: store.go Open() and gitFor() still resolve via realPath at HEAD; kb tests open repos through a symlink
