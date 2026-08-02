---
type: Operations
title: Development and Operations
description: Development setup, common commands, database migrations, testing strategy, code generation, worktree support, self-hosting, and CI pipeline for the Multica repository.
tags: [development, operations, setup, testing, migrations, self-hosting, ci]
---

# Development and Operations

## Prerequisites

- **Go 1.26.1** (CI version; earlier 1.2x may work)
- **Node 22** (CI version)
- **pnpm 10.28.2** (exact version; managed by `packageManager` in `package.json`)
- **Docker** with Compose CLI plugin (for PostgreSQL, Redis, self-hosting)
- **PostgreSQL** with `pgvector` extension (CI uses `pgvector/pgvector:pg17`)

## Initial Setup

```bash
# Clone and bootstrap
git clone https://github.com/multica-ai/multica.git
cd multica

# Auto-setup: creates .env, starts PostgreSQL + Redis, runs migrations, installs deps, starts server + frontend
make dev
```

`make dev` auto-detects worktree checkouts and isolates them from the main checkout (separate DB, ports, environment).

## Common Commands

### Backend (Go)

```bash
make server           # Run Go API server only
make daemon           # Run local daemon
make test             # Run Go tests
make sqlc             # Regenerate sqlc code after changing SQL queries
make migrate-up       # Run pending migrations
make migrate-down     # Roll back last migration
```

### Frontend (TypeScript)

```bash
pnpm install          # Install dependencies
pnpm dev:web          # Start Next.js web app
pnpm dev:desktop      # Start Electron desktop app
pnpm build            # Build all packages and apps (except mobile)
pnpm typecheck        # TypeScript type checking across all packages
pnpm test             # Vitest unit tests (via Turborepo)
pnpm lint             # ESLint across all packages
pnpm exec playwright test  # End-to-end tests
```

### Full Verification

```bash
make check            # Runs Go tests + pnpm typecheck + pnpm test + pnpm lint
```

## Database Migrations

Migrations are versioned SQL files in `server/migrations/`. Naming convention: `NNN_description.up.sql` and `NNN_description.down.sql`.

### Key Facts

- **150+ migrations** covering the full schema evolution
- Sequential numbering enforced by `server/internal/migrations/migrations_lint_test.go`
- Migration numbers can be renumbered to resolve conflicts (`6a72f248a` — unblocked release by renumbering 145→150, 146→152, etc.)
- **sqlc** generates type-safe Go code from SQL queries:
  - Queries in `server/pkg/db/queries/` (40+ files, organized by domain)
  - Generated output in `server/pkg/db/generated/`
  - Config in `server/sqlc.yaml`
- After changing SQL queries, run `make sqlc` to regenerate
- Migration runner: `server/cmd/migrate/`

### Adding a Migration

1. Create `server/migrations/NNN_description.up.sql` and `NNN_description.down.sql`
2. Update affected query files in `server/pkg/db/queries/`
3. Run `make sqlc` to regenerate Go code
4. Run `make test` to verify lint and generated code

## Testing Strategy

Tests follow the code with a clear location convention:

| What | Where | Framework |
|------|-------|-----------|
| Go unit + integration tests | `server/` — alongside source files | `go test` |
| Shared business logic, stores, queries, hooks | `packages/core/*.test.ts` | Vitest |
| Shared UI components, pages, forms, modals | `packages/views/*.test.tsx` | Vitest |
| Platform wiring (cookies, redirects, search params) | `apps/web/*.test.tsx` | Vitest |
| End-to-end flows | `e2e/*.spec.ts` | Playwright |
| Migration ordering | `server/internal/migrations/migrations_lint_test.go` | `go test` |

### Testing Rules

- Never test shared component behavior in an app test file
- `packages/views/` tests must not mock `next/*` or `react-router-dom`
- Mock `@multica/core` stores with the Zustand callable-store shape
- Mock `@multica/core/api` for API calls
- E2E tests use `TestApiClient` for setup/teardown
- Go tests: `make test` with `-race` flag automatically applied; `pkg/agent` tests have concurrency capped under race detector (`78591f602`)

### E2E Test Coverage

The `e2e/` directory covers: agent MCP, auth, chat attachments, comments, issues, navigation, onboarding, settings.

## Worktree Support

The repo supports git worktrees for parallel development:

```bash
make worktree-env     # Generate .env.worktree with isolated ports/DB
make setup-worktree   # Full setup for worktree checkout
make start-worktree   # Start processes for worktree
```

Worktrees share one PostgreSQL container and get isolated DB names and ports via `.env.worktree`. `make dev` auto-detects the active environment.

## Self-Hosting

Multica supports self-hosted deployment via Docker Compose:

```bash
make selfhost         # Create .env, pull images, start all services
```

Services in `docker-compose.selfhost.yml`:
- PostgreSQL with pgvector
- Redis
- Multica API server
- Multica web frontend

Additional documentation: `SELF_HOSTING.md`, `SELF_HOSTING_ADVANCED.md`, `SELF_HOSTING_AI.md`.

### Configuration

Copy `.env.example` to `.env` and customize. Key variables:

| Variable | Purpose |
|----------|---------|
| `DATABASE_URL` | PostgreSQL connection string |
| `JWT_SECRET` | JWT signing secret |
| `MULTICA_SERVER_URL` | WebSocket URL for daemon connections |
| `MULTICA_LLM_API_KEY` | LLM API key for chat title generation and internal LLM features |
| `MULTICA_LARK_SECRET_KEY` | Lark/Feishu integration master key |
| `COMPOSIO_API_KEY` | Composio tool connector API key |

The generic LLM passthrough endpoints (`/api/llm/*`) were removed in `619b1b78e` (MUL-4309). LLM access is now internal-only (chat title generation).

## CI Pipeline

CI runs via GitHub Actions (`.github/workflows/ci.yml`):

- **OS**: Ubuntu
- **Go**: 1.26.1
- **Node**: 22
- **Database**: PostgreSQL 17 with pgvector
- **Pipeline**: Lint → Test (Go + TypeScript + E2E) → Build

The scheduled OpenWiki workflow (`.github/workflows/openwiki-update.yml`) refreshes this wiki periodically.

## Release Process

Releases use GoReleaser (`.goreleaser.yml`):

1. Commit with conventional prefix: `feat(scope)`, `fix(scope)`, `refactor(scope)`, etc.
2. Create and push a version tag: `v0.x.x`
3. `release.yml` publishes binaries and updates the Homebrew tap
4. Bump patch by default unless specified

## Code Generation

| Tool | Purpose | Command |
|------|---------|---------|
| sqlc | Generate Go DB query code from SQL | `make sqlc` |
| shadcn/ui | Add UI components to packages/ui | `pnpm ui:add <component>` |
| Reserved slugs | Generate reserved slug list from JSON | `pnpm generate:reserved-slugs` |

## Related Concepts

- [Architecture Overview](../architecture/overview.md) — system design and component layers
- [Task Execution Workflow](../workflows/task-execution.md) — the workflow these operations support
