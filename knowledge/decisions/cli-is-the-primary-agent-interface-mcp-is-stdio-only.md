---
type: Decision
title: CLI is the primary agent interface; MCP is stdio-only and optional
description: Shell commands cost no context until used; the MCP server exposes five tools over stdio and never listens on the network.
tags: [mcp, design]
status: draft
resources: [internal/mcp/**, internal/cli/**]
generated: {by: claude-code, at: "2026-11-08T05:42:13Z", commit: add3b75b68f0}
---
Every MCP tool schema is paid for in every session, while the four main agents can all run shell commands. A network MCP endpoint would be an unauthenticated write path. See docs/ARCHITECTURE.md section 2.5.
