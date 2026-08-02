---
type: Quickstart
title: Multica Code Wiki
description: "Entrypoint to the Multica code wiki. Covers the managed agents platform where coding agents become first-class teammates — assign issues, track progress, compound skills. Navigate to architecture, task execution, agents, collaborations, integrations, and operations."
tags: [quickstart, overview, navigation]
---

# Multica Code Wiki

Multica is an open-source managed agents platform. It turns coding agents into real teammates — assign tasks, track progress, and compound skills. The platform supports 15+ agent CLIs (Claude Code, Codex, CodeBuddy, Copilot CLI, OpenCode, Cursor Agent, and more), runs them through a local daemon, and provides a full web/desktop/mobile dashboard.

This wiki documents the repository at a level useful for engineers working on the codebase and for future agents maintaining documentation.

## Repository Shape

| Layer | Technology | Location |
|-------|-----------|----------|
| Backend API server | Go (Chi router, sqlc, PostgreSQL, Redis, WebSockets) | `server/` |
| Daemon + CLI | Go (Cobra, agent process management) | `server/cmd/multica/`, `server/internal/daemon/` |
| Web frontend | Next.js App Router, React 19, Tailwind | `apps/web/` |
| Desktop app | Electron + Vite | `apps/desktop/` |
| Mobile app | Expo / React Native | `apps/mobile/` |
| Shared business logic | TypeScript (TanStack Query, Zustand, Zod) | `packages/core/` |
| Shared UI components | React (shadcn/Base UI) | `packages/ui/` |
| Shared view pages | React (feature pages, layouts) | `packages/views/` |
| Documentation site | Next.js | `apps/docs/` |

The monorepo uses **pnpm workspaces + Turborepo** for frontend packages and standard **Go modules** for the backend.

## Documentation Map

- [Architecture Overview](architecture/overview.md) — System architecture, component layers, event-driven design, WebSocket realtime, package boundaries, state management split, and frontend monorepo structure
- [Task Execution Workflow](workflows/task-execution.md) — The core agent task lifecycle: issue assignment → task enqueue → daemon claim → agent CLI execution → progress streaming → completion
- [Agents and Runtimes](domain/agents-and-runtimes.md) — Agent creation and configuration, runtime types (15+ CLIs), model discovery, thinking levels, skill binding, daemon management
- [Issues and Collaboration](domain/issues-and-collaboration.md) — Issue management with polymorphic assignees, comments and mentions, squad routing, chat sessions, autopilots
- [Integrations](integrations/overview.md) — Slack, Lark/Feishu, GitHub, Composio, webhooks, IM channel abstraction
- [Development and Operations](operations/development.md) — Setup, common commands, database migrations, testing, self-hosting, CI

## Key Architectural Decisions

**Event-driven backend.** HTTP handlers publish events to an in-process `events.Bus`. Subscribers handle side-effects (notifications, realtime broadcast, autopilot triggers). Events fan out through Redis streams to WebSocket hubs for cross-process delivery.

**Polymorphic assignees.** Issues can be assigned to either human members or AI agents. The `assignee_type` + `assignee_id` pattern allows agents to be first-class participants in the issue lifecycle — they claim work, post comments, change statuses, and create child issues.

**Daemon model.** A local daemon process connects to the server via WebSocket, registers available agent runtimes (CLIs found on the machine), and receives task dispatches. The daemon manages the agent process lifecycle, captures output, and streams progress back to the server.

**Package boundary rules.** Frontend code enforces strict boundaries: `packages/core/` has zero UI dependencies, `packages/ui/` has zero business logic, and `packages/views/` has zero platform-specific imports (`next/*`, `react-router-dom`). See [Architecture Overview](architecture/overview.md) for details.

## Key Commands

```bash
make dev              # Auto-setup and start everything
make test             # Go tests
pnpm test             # TypeScript unit tests (Vitest)
pnpm typecheck        # TypeScript type checking
pnpm exec playwright test  # End-to-end tests
make check            # Full verification pipeline
make sqlc             # Regenerate sqlc code after SQL changes
```

## Quick Source Navigation

| What | Where |
|------|-------|
| API entrypoint + routing | `server/cmd/server/router.go` |
| Handler (all HTTP endpoints) | `server/internal/handler/` (180+ files) |
| Business logic services | `server/internal/service/` |
| Daemon core loop | `server/internal/daemon/daemon.go` |
| Agent runtime adapters | `server/pkg/agent/` (models.go, thinking.go, claude.go, codex.go, etc.) |
| WebSocket wire protocol | `server/pkg/protocol/messages.go` |
| Database queries (sqlc) | `server/pkg/db/queries/` |
| Database generated code | `server/pkg/db/generated/` |
| Migrations | `server/migrations/` |
| API client (frontend) | `packages/core/api/client.ts` |
| Issue queries + mutations | `packages/core/issues/` |
| View components | `packages/views/` per domain |
| E2E tests | `e2e/` |

## Backlog

| Area | Source Anchor | Reason Deferred |
|------|---------------|-----------------|
| Mobile app | `apps/mobile/` | Has its own CLAUDE.md; independently maintained with separate release cadence |
| Desktop specifics | `apps/desktop/src/` | Covered at high level in frontend/overview; Electron-specific details deferred |
| Analytics architecture | `docs/analytics.md` | Extensive existing docs cover this domain |
| Skill authoring guide | `server/internal/service/builtin_skills/` | Built-in skills are self-documenting; custom skill authoring needs separate deep-dive |
| Feature flag system | `server/pkg/featureflag/` | YAML-configured percentage rollouts; referenced in architecture |
| Billing and pricing | `server/internal/metrics/pricing.go` | Model pricing and usage tracking; deferred |
