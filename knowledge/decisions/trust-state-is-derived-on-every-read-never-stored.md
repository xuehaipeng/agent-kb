---
type: Decision
title: Trust state is derived on every read, never stored
description: unverified/corroborated/reviewed/disputed/deprecated come from the append-only verified list, supersedes links and git; only OKF status is written, by the tool.
tags: [trust, design]
status: draft
resources: [internal/kb/trust.go]
generated: {by: claude-code, at: "2026-11-08T05:42:13Z", commit: add3b75b68f0}
---
A stored trust_state field (as in the AgentKB draft) is a second source of truth that drifts from the verification trail. Store.trust() recomputes the state; applyStatus keeps OKF status consistent and lint warns on mismatch. See docs/ARCHITECTURE.md sections 2.1-2.2.
