# agent-kb development notes

- Go 1.22+, single dependency (`gopkg.in/yaml.v3`). Build: `make build`; test: `make test`; format: `gofmt -w .`.
- Packages: `internal/okf` (frontmatter round-trip, keeps unknown keys), `internal/kb` (store, trust derivation, search, reviews, lint, index), `internal/cli`, `internal/mcp`, `internal/extract`, `internal/install` (embedded templates).
- Every bundle mutation goes through a `kb.Store` method that holds `Store.Lock()` and writes atomically; never write node files directly.
- Generated outputs (`index.md`) must be deterministic: no clock or git-HEAD dependence (use `TrustStatic`).
- Two modes share one `kb.Store`: repo mode (`<repo>/knowledge`, node IDs like `learnings/x`) and hub mode (`~/.kb`, IDs like `<project>/learnings/x`, `global/...`). Test both when touching store, search, index or lint.
- Design rationale lives in `docs/ARCHITECTURE.md`; update it when you change a decision.

<!-- kb:begin (managed by 'kb install'; re-run it to update, edits here are overwritten) -->
## Shared knowledge (kb)

You share a knowledge base with the other coding agents (Claude Code, Codex, Hermes, ...) through the `kb` CLI. It knows which project the current repo belongs to. Use it so nobody re-discovers the same things, and leave what you learn for the next session.

- Before working on unfamiliar code or answering architecture questions: `kb search "<terms>" --file <path>` (this project + global; `--all` for every project), then `kb show <id>` for relevant hits only.
- `verified`, `cross-checked` and `reviewed` entries are backed by evidence. `unverified`, `disputed` and `stale` ones are leads: check them against the code first.
- Learned something durable and non-obvious (root cause, gotcha, invariant, decision, procedure)? `kb add <fact|learning|decision|convention> "<title>" -d "<one line>" -r "<code globs>" -b "<details>" -e "<how you know>"`. Add `--global` when it spans projects.
- Found an entry stale or wrong? Fix it in place: `kb update <id> -b "<corrected text>" -m "<why>" -e "<evidence>"`, or re-confirm it (`kb verify <id> --confirm -e "..."`), or retire it (`kb deprecate <id> -m "<why>"`).
- Optional cross-review of a non-trivial commit range: `kb review submit "<summary>"`; others run `kb review list --for-me`.
- Never edit knowledge files by hand and never store secrets. `kb help` lists everything.
<!-- kb:end -->
