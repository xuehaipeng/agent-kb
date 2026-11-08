---
name: kb
description: Shared knowledge base for the user's coding projects (kb CLI over a git-synced hub). Use before answering project questions or triaging issues, and to record findings for the coding agents.
---

# kb on the Hermes host

All projects' knowledge lives in the hub (`~/.kb`, a git clone shared with the
user's laptop). You are usually not inside a project repo, so name the project.

1. Refresh first: `kb sync --no-push`.
2. Look up before answering: `kb search "<terms>" --all` (or `-p <project>`),
   then `kb show <id>`. `kb project list` shows the projects. Treat
   `unverified`, `disputed` and `stale` entries as leads and say so.
3. Record durable findings (incident root causes, decisions the user tells you):
   `kb add learning "<title>" -d "<one line>" -b "<details>" -p <project> [-e "<evidence>"]`,
   or `--global` for cross-project knowledge.
4. Fix entries you know are outdated: `kb update <id> -b "..." -m "<why>"`.
5. Status for the user: `kb status --all`, `kb review list --all`.
6. Publish: `kb sync`.

Never store secrets. Never edit knowledge files by hand.
