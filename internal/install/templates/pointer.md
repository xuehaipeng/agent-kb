## Shared knowledge (kb)

You share a knowledge base with the other coding agents (Claude Code, Codex, Hermes, ...) through the `kb` CLI. It knows which project the current repo belongs to. Use it so nobody re-discovers the same things, and leave what you learn for the next session.

- Before working on unfamiliar code or answering architecture questions: `kb search "<terms>" --file <path>` (this project + global; `--all` for every project), then `kb show <id>` for relevant hits. Results also list matching sections of the repo's own docs (`doc path:line`); read those directly.
- `verified`, `cross-checked` and `reviewed` entries are backed by evidence; `unverified`, `disputed` and `stale` ones are leads to check against the code.
- Learned something durable and non-obvious (root cause, gotcha, invariant, decision, procedure) that the repo's docs don't already say? `kb add <fact|learning|decision|convention> "<title>" -d "<one line>" -r "<code globs>" -b "<details>" -e "<how you know>"` (`--global` if it spans projects). Never copy docs; if a doc is wrong, fix the doc.
- Found an entry stale or wrong? `kb update <id> -b "<corrected text>" -m "<why>" -e "<evidence>"`, re-confirm it (`kb verify <id> --confirm -e "..."`), or retire it (`kb deprecate <id> -m "<why>"`).
- Optional cross-review of a commit range: `kb review submit "<summary>"`; others run `kb review list --for-me`.
- Never edit knowledge files by hand or store secrets. `kb help` lists everything.
