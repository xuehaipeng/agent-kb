# Knowledge bundle

Shared, git-backed knowledge for the coding agents that work on this repository
(Claude Code, Codex, Hermes, Cursor, ...), managed by the `kb` CLI from
[agent-kb](https://github.com/xuehaipeng/agent-kb).

- Each file is one claim in [Open Knowledge Format v0.2](https://github.com/GoogleCloudPlatform/open-knowledge-format):
  Markdown with YAML frontmatter recording who generated it (`generated`), who
  checked it (`verified`), when it goes stale (`stale_after`) and which code it
  is about (`resources`).
- A claim becomes **corroborated** only when an agent from a *different model
  family* confirms it against the code, or **reviewed** when a human does.
  Disputes are resolved by a human: `kb resolve <id> --keep|--deprecate --as human:<you>`.
- `index.md` files and `log.md` are generated/append-only. Don't edit them.
- Machine-local state lives in `.kb/` (gitignored).

Common commands:

```
kb status                     # health: disputes, stale claims, open reviews
kb search "query" --file src/x.go
kb show <id>
kb add learning "Title" -d "one line" -r "src/**" -b "details"
kb verify <id> --confirm -e "what you checked"
kb review list --for-me
kb lint && kb index
```

Types and their lifecycles are configured in `kb.yaml`.
