## Shared knowledge (kb)

You share a knowledge base with the other coding agents (Claude Code, Codex, Hermes, ...) through the `kb` CLI. It knows which project the current repo belongs to. Use it so nobody re-discovers the same things, and leave what you learn for the next session.

- Before working on unfamiliar code or answering architecture questions: `kb search "<terms>" --file <path>` (this project + global; `--all` for every project), then `kb show <id>` for relevant hits only.
- `verified`, `cross-checked` and `reviewed` entries are backed by evidence. `unverified`, `disputed` and `stale` ones are leads: check them against the code first.
- Learned something durable and non-obvious (root cause, gotcha, invariant, decision, procedure)? `kb add <fact|learning|decision|convention> "<title>" -d "<one line>" -r "<code globs>" -b "<details>" -e "<how you know>"`. Add `--global` when it spans projects.
- Found an entry stale or wrong? Fix it in place: `kb update <id> -b "<corrected text>" -m "<why>" -e "<evidence>"`, or re-confirm it (`kb verify <id> --confirm -e "..."`), or retire it (`kb deprecate <id> -m "<why>"`).
- Optional cross-review of a non-trivial commit range: `kb review submit "<summary>"`; others run `kb review list --for-me`.
- Never edit knowledge files by hand and never store secrets. `kb help` lists everything.
