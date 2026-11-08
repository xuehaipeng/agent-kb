# agent-kb development notes

- Go 1.22+, single dependency (`gopkg.in/yaml.v3`). Build: `make build`; test: `make test`; format: `gofmt -w .`.
- Packages: `internal/okf` (frontmatter round-trip, keeps unknown keys), `internal/kb` (store, trust derivation, search, reviews, lint, index), `internal/cli`, `internal/mcp`, `internal/extract`, `internal/install` (embedded templates).
- Every bundle mutation goes through a `kb.Store` method that holds `Store.Lock()` and writes atomically; never write node files directly.
- Generated outputs (`index.md`) must be deterministic: no clock or git-HEAD dependence (use `TrustStatic`).
- Design rationale lives in `docs/ARCHITECTURE.md`; update it when you change a decision.

<!-- kb:begin (managed by 'kb install'; re-run it to update, edits here are overwritten) -->
## Shared knowledge (kb)

This repo keeps cross-agent knowledge in `knowledge/`, shared by Claude Code, Codex, Hermes and other agents through the `kb` CLI. Every claim carries a trust state; retrieve it on demand instead of guessing.

- Before changing code or answering architecture questions: `kb search "<terms>" --file <path you will touch>`, then `kb show <id>` for relevant hits only.
- `corroborated`/`reviewed` = checked by another model family or a human. `unverified`, `disputed`, `stale` = hypotheses: check them against the code before relying on them.
- When you have checked a claim against the code: `kb verify <id> --confirm|--dispute -e "<file/test/command you checked>"`. You cannot verify claims from your own model family.
- Learned something durable and non-obvious (root cause, gotcha, invariant, decision)? `kb add <fact|learning|decision|convention> "<title>" -d "<one line>" -r "<code globs>" -b "<details>"`. To change a claim, add a new one with `--supersedes <id>`.
- After a non-trivial change is committed: `kb review submit "<summary>" -a "<assumption>"`. To review others' work: `kb review list --for-me`, `kb review show <id>`, then `kb review verdict <id> --approve|--changes -f "<finding>"`.
- Never edit files under `knowledge/` by hand and never store secrets there. `kb help` lists all commands.
<!-- kb:end -->
