## Shared knowledge (kb)

This repo keeps cross-agent knowledge in `knowledge/`, shared by Claude Code, Codex, Hermes and other agents through the `kb` CLI. Every claim carries a trust state; retrieve it on demand instead of guessing.

- Before changing code or answering architecture questions: `kb search "<terms>" --file <path you will touch>`, then `kb show <id>` for relevant hits only.
- `corroborated`/`reviewed` = checked by another model family or a human. `unverified`, `disputed`, `stale` = hypotheses: check them against the code before relying on them.
- When you have checked a claim against the code: `kb verify <id> --confirm|--dispute -e "<file/test/command you checked>"`. You cannot verify claims from your own model family.
- Learned something durable and non-obvious (root cause, gotcha, invariant, decision)? `kb add <fact|learning|decision|convention> "<title>" -d "<one line>" -r "<code globs>" -b "<details>"`. To change a claim, add a new one with `--supersedes <id>`.
- After a non-trivial change is committed: `kb review submit "<summary>" -a "<assumption>"`. To review others' work: `kb review list --for-me`, `kb review show <id>`, then `kb review verdict <id> --approve|--changes -f "<finding>"`.
- Never edit files under `knowledge/` by hand and never store secrets there. `kb help` lists all commands.
