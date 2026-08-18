---
id: multica-architecture
type: ARCHITECTURE
title: Multica Architecture Map
status: living
owner: Ray
created: 2026-08-17T19:00:11+08:00
updated: 2026-08-17T19:00:11+08:00
---

# ARCHITECTURE.md - Multica

> This is the repository map for stable module boundaries and dependency direction.
> Implementation details remain authoritative in code. Hard engineering rules remain authoritative in `CLAUDE.md`.

## 1. Bird's Eye View

Multica is an AI-native task management platform. A Go server owns business state, authentication, task dispatch, realtime events, and daemon APIs. Local daemons claim tasks and run provider CLIs. Web and desktop share headless logic and business views through workspace packages; mobile keeps an independent UI and runtime integration.

Core flow:

```text
web / desktop / mobile / CLI
  -> Go HTTP API + PostgreSQL
  -> agent_task_queue
  -> authenticated local daemon
  -> provider CLI + injected skills
  -> task result + domain events
  -> PostgreSQL projections + realtime clients
```

## 2. Entry Points

| Concern | Start here |
|---|---|
| Server process and lifecycle | `server/cmd/server/main.go` |
| HTTP composition and routes | `server/cmd/server/router.go` |
| Business handlers and services | `server/internal/handler/`, `server/internal/service/` |
| Daemon claim and execution | `server/internal/daemon/`, `server/internal/handler/daemon.go` |
| Database schema and queries | `server/migrations/`, `server/pkg/db/queries/` |
| Shared frontend logic and API | `packages/core/` |
| Shared business UI | `packages/views/` |
| Atomic UI primitives | `packages/ui/` |
| Fork-specific additions | `CUSTOM.md`, `server/internal/governance/` |

## 3. Code Map

### `server/cmd/`

Process entry points and composition roots. Wiring belongs here; reusable business behavior does not.

### `server/internal/handler/`

HTTP request boundaries: authentication-derived identity, input validation, workspace authorization, response encoding, and delegation to services.

### `server/internal/service/`

Business operations and transactional coordination for issues, tasks, agents, chat, projects, and automations. Services publish committed outcomes through the in-process event bus.

### `server/internal/governance/`

AI-First fork governance: CR event projection, signed approvals, gate/run projections, and generated read-only contracts. Git remains authoritative for CR state; PostgreSQL rows are operational state and replayable projections.

### `server/internal/daemon/`

Local execution control plane: task claim, isolated environment preparation, skill delivery, provider CLI launch, progress, completion, and local CR event/grant transport.

### `server/pkg/db/`

SQL source under `queries/` and generated sqlc code under `generated/`. Generated files are never hand-edited.

### `packages/core/`, `packages/ui/`, `packages/views/`

`core` owns headless client logic and server-state queries; `ui` owns atomic primitives; `views` composes business UI. Web and desktop consume these packages. Mobile owns its UI and imports only supported pure/type surfaces from core.

## 4. Dependency Direction

```text
apps + server/cmd              composition and platform entry points
  -> handlers / shared views
  -> services / packages/core
  -> db queries, domain helpers, packages/ui
  -> PostgreSQL, provider CLIs, external APIs
```

Rules:

- Server handlers may call services; services must not depend on handlers.
- The daemon consumes server task contracts; it does not become a second server-state authority.
- `packages/views` may depend on `packages/core` and `packages/ui`; those packages must not depend on views.
- Platform navigation stays in app platform adapters, not shared views.
- Fork governance may consume existing services and events, but must not duplicate task execution, Git transactions, or CR state machines.

## 5. Hard Invariants

Violation of any item is a review blocker.

1. **Workspace isolation**: every query and task operation is scoped by authoritative workspace identity. Request-body workspace identifiers never override authenticated context.
2. **Server/client state split**: TanStack Query owns server state; Zustand owns client/view state. Realtime events update or invalidate query state rather than mirroring server payloads into stores.
3. **Task single path**: Agent work executes through `agent_task_queue`, TaskService, daemon claim, and task-scoped credentials. New orchestration reuses this path.
4. **CR authority split**: Git and `crctl` own CR status, gates, approvals, and controlled file writes. Multica stores operational state and projections; it never writes CR authority files directly.
5. **Generated sources**: sqlc and governance generated Go files are regenerated from their declared source, never manually patched.
6. **Migration safety**: new relationships use application validation instead of foreign keys; every new index is built with `CREATE [UNIQUE] INDEX CONCURRENTLY` in its own single-statement migration.
7. **No hidden machine approval**: machine credentials cannot satisfy human-only approval boundaries; signed grants or interactive human approval remain mandatory.
8. **API compatibility**: frontend network responses are schema-parsed, enum switches have defaults, and installed desktop clients tolerate additive backend response drift.
9. **English code comments**: all new or changed source-code comments are English.

## 6. Negative Space

| Do not add | Reason | Reconsider when |
|---|---|---|
| A second task queue or agent runtime | Existing queue, daemon, retry, and task-token path already provide the execution boundary | The current task model cannot represent a proven production case |
| A second CR state machine or Git writer | `crctl` and Git are the authority; parallel writers would split recovery semantics | Never without an explicit platform redesign |
| Generic workflow/DAG infrastructure for one fixed flow | Fixed domain flows are cheaper and easier to verify | Multiple proven flows require the same non-linear semantics |
| Frontend-specific business logic in app routers | It breaks web/desktop sharing | The behavior genuinely depends on platform APIs |
| Compatibility shims for internal pre-release code | They create dual paths without an installed-client constraint | A real external compatibility boundary requires one |

## 7. Cross-Cutting Concerns

- **Errors**: handlers return bounded structured errors; services wrap checked errors; fail closed at authentication, workspace, and authority boundaries.
- **Transactions**: use the existing service/pgx transaction boundaries. Side effects publish only after committed state.
- **Events**: `internal/events.Bus` is synchronous in-process notification, not durable authority. PostgreSQL/Git facts must make handlers replayable.
- **Security**: task tokens bind user, Agent, task, and workspace. Sensitive values stay server/daemon-side and out of logs.
- **Testing**: Go behavior lives next to the owning package; DB tests must actually run against PostgreSQL rather than pass through a skipped TestMain.
- **Configuration**: environment variables configure process boundaries; malformed security configuration fails startup rather than silently disabling enforcement.
- **Fork tracking**: every AI-First code customization is recorded in `CUSTOM.md` and marked in source where the file format permits it.

## 8. Maintenance

Update this file only when a top-level module, dependency direction, or hard invariant changes. Ordinary feature work should update its local documentation and tests instead. Technical design review must check sections 4-6 before approval.
