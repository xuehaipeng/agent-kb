## Shared knowledge (kb)

You share a knowledge base with the other coding agents (Claude Code, Codex, Hermes, ...) through the `kb` CLI. It knows which project the current repo belongs to. Use it so nobody re-discovers the same things, and leave what you learn for the next session.

- Follow the rules from `kb rules` (Claude Code receives them at session start; otherwise run it when you start a task). When the user states a lasting rule or preference, record it: `kb add rule "<instruction>" -d "<why>" -e "user said so"` (`--global` if it applies everywhere).
- Before working on unfamiliar code: `kb search "<terms>" --file <path>` (`--all` for every project), then `kb show <id>`. Results also list matching sections of the repo's docs (`doc path:line`); read those directly.
- `verified`, `cross-checked` and `reviewed` entries are backed by evidence; `unverified`, `disputed` and `stale` ones are leads to check.
- Learned something durable and non-obvious the docs don't say? `kb add <fact|learning|decision> "<title>" -d "<one line>" -r "<code globs>" -b "<details>" -e "<how you know>"`. Never copy docs; fix a wrong doc instead.
- Entry stale or wrong? `kb update <id> -b "<corrected>" -m "<why>" -e "<evidence>"`, `kb verify <id> --confirm -e "..."`, or `kb deprecate <id> -m "<why>"`.
- Optional cross-review: `kb review submit "<summary>"`; others run `kb review list --for-me`.
- Never edit knowledge files by hand or store secrets. `kb help` lists everything.
