---
name: kb
description: Shared knowledge base for the user's coding projects (kb CLI over a git clone). Use before answering project questions or triaging issues, and to record findings for the local coding agents.
---

# kb on the Hermes host

Project knowledge lives in a git clone on this host (`$KB_ROOT`, a repo's
`knowledge/` directory). Local agents (Claude Code, Codex) verify and review;
you mostly search, triage, and propose.

1. Refresh first: `kb sync --no-push` (pulls the latest knowledge).
2. Look up before answering: `kb search "<terms>"`, then `kb show <id>`.
   Treat `unverified`, `disputed` and `stale` entries as hypotheses and say so.
3. Record durable findings (log root causes, incident facts, decisions the user
   tells you): `kb add learning "<title>" -d "<one line>" -b "<details>" -r "<code globs>"`.
   They start `unverified`; a local agent from another model family checks them.
4. You may verify claims from other model families when you checked the
   evidence: `kb verify <id> --confirm|--dispute -e "<what you checked>"`.
5. Publish: `kb sync` (commits only the knowledge directory and pushes).

Never store secrets. Never edit knowledge files by hand.
