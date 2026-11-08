# Knowledge hub

Shared knowledge for the coding agents (Claude Code, Codex, Hermes, Cursor, ...)
across all projects, managed by the `kb` CLI from
[agent-kb](https://github.com/xuehaipeng/agent-kb).

```
kb.yaml                  types, TTLs, model families, autocommit
global/                  knowledge shared by every project
projects/<id>/           one directory per project
  project.yaml           repositories that belong to the project
  facts/ learnings/ decisions/ conventions/ reviews/
index.md, log.md         generated / append-only
.kb/                     machine-local (registry of checkouts, locks): gitignored
```

- `kb` maps the current repository to a project through its git remote;
  `kb project link <id>` groups several repos (or a fork) into one project.
- Every `kb` write is committed here automatically; `kb sync` pulls and pushes
  so other machines (and the Hermes host) see it.
- Each entry is [Open Knowledge Format](https://github.com/GoogleCloudPlatform/open-knowledge-format)
  Markdown: who wrote it, who checked it (`verified`), what code it depends on
  (`resources`) and when it goes stale.

Don't edit files by hand; use `kb add`, `kb update`, `kb verify`, `kb deprecate`.
