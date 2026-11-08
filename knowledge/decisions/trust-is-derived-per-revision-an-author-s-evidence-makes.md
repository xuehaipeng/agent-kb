---
type: Decision
title: Trust is derived per revision; an author's evidence makes an entry verified
description: verified/cross-checked/reviewed/unverified/disputed/deprecated are computed from the verified events of the current revision; the latest event wins; nothing stores the state.
tags: [trust, design]
status: deprecated
resources: [internal/kb/trust.go]
supersedes: [decisions/trust-state-is-derived-on-every-read-never-stored]
generated: {by: claude-code, at: "2026-11-08T13:56:16Z", commit: a8425826899a}
verified:
  - by: claude-code
    at: "2026-11-08T13:56:16Z"
    result: confirmed
    rev: 1
    commit: a8425826899a
    evidence: internal/kb/trust.go trust(); TestAddVerifyLifecycle, TestSelfVerifyUpdateAndDeprecate
---
Knowledge sharing comes first: most tasks are done by one agent, so requiring a second model family before anything counts was too strict. An agent that checked a claim records it with evidence and it is verified; a different family upgrades it to cross-checked, a human to reviewed. Events carry the revision they checked, so an update starts the new content fresh while the old checks stay in the trail. Supersedes the stricter cross-family-only rule (docs/ARCHITECTURE.md section 2.2).
