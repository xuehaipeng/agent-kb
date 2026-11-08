# Knowledge bundle

Shared, git-backed knowledge for the coding agents that work on this repository
(Claude Code, Codex, Hermes, Cursor, ...), managed by the `kb` CLI from
[agent-kb](https://github.com/xuehaipeng/agent-kb). This is *repo mode*: the
knowledge is versioned with the code. For many repos, a multi-project hub
(`kb hub init`) keeps everything in one place instead.

- Each file is one entry in [Open Knowledge Format v0.2](https://github.com/GoogleCloudPlatform/open-knowledge-format):
  Markdown with YAML frontmatter recording who wrote it (`generated`), who
  checked it and on which revision (`verified`), when it goes stale
  (`stale_after`) and which code it is about (`resources`).
- An entry with evidence is `verified`; confirmation by a different model
  family makes it `cross-checked`, by a human `reviewed`. Stale or wrong
  entries are fixed in place with `kb update` (a new revision) or retired with
  `kb deprecate`.
- `index.md` files and `log.md` are generated/append-only. Don't edit them.
- Machine-local state lives in `.kb/` (gitignored).

Common commands:

```
kb status                     # health: disputes, stale entries, open reviews
kb search "query" --file src/x.go
kb show <id>
kb add learning "Title" -d "one line" -r "src/**" -b "details" -e "how you know"
kb update <id> -b "corrected text" -m "why" -e "evidence"
kb verify <id> --confirm -e "what you checked"
kb lint
```

Types and their lifecycles are configured in `kb.yaml`.
