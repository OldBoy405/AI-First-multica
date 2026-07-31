# CUSTOM.md — AI-First fork 二开台账

逐条记录本 fork 相对上游（`multica-ai/multica`）的所有定制。每次双周 rebase 前先核对本表（配合 `grep -rn "// AIFIRST:" server/ apps/`，见 [CONTRIBUTING.AIFIRST.md](CONTRIBUTING.AIFIRST.md) 规则二、三）。

## 代码改动

| # | 位置 | 改动 | 原因 / 追溯 | 日期 |
|---|---|---|---|---|
| 1 | `server/cmd/server/router.go`（2 处，均有 `// AIFIRST:` 标记） | 摘除 Stripe webhook（`/api/webhooks/stripe`）与 `/api/cloud-billing` 路由组的挂载；handler 代码保留未删 | 内网自托管无云端计费；访问即 404。CR-2026-001 FR-1（AI First Platform 仓库 `change-requests/CR-2026-001/`） | 2026-07-30 |
| 2 | `server/pkg/agent/claude.go#isFilteredChildEnvKey`（`// AIFIRST:` 标记） | 过滤名单补入 `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST` 与 `CLAUDE_CODE_HOST_AUTH_ENV_VAR` 两个宿主托管认证标记 | 二分实证：两者任一单独泄漏给 daemon 拉起的 claude 子进程即报 `Not logged in`（凭据文件明明有效）；同批的 CHILD_SESSION/HOST_SESSION_ID/SDK_HAS_*_REFRESH 单独存在无害、未动。**候选回馈上游 PR**（上游注释明说该名单按名维护、欢迎补全）。CR-2026-001 TASK-04 | 2026-07-31 |
| 3 | `server/migrations/158_aifirst_cr_projection.{up,down}.sql`（新文件） | 新增治理投影三表：`cr` / `cr_sync_event` / `approval_record`（approve 用部分唯一索引 `WHERE decision='approve'`，reject 多条留痕） | P0 数据模型映射（git 权威 / PG 投影）落地；表可清空重放。rebase 时保持迁移编号顺延、勿与上游新迁移撞号。CR-2026-002 TASK-04 | 2026-07-31 |
| 4 | `server/internal/governance/`（新包：`actions.go`、`transitions_gen.go`、`gen/generate-transitions.mjs`、测试） | 治理层自研包：activity_log 两个 `aifirst.` action 常量 + CR 状态机只读副本（45 条展开转移，生成产物入库，`gen --check` 守一致性）+ 后续 crsync/approval/reconcile 落此包 | 规则一（自研代码住新目录）；构建不依赖 tools checkout（SDD-SUG-003）。状态机变更流程：改 tools dir-graph.yaml → 重跑 gen → 提交两仓。CR-2026-002 TASK-04 | 2026-07-31 |
| 5 | `server/internal/governance/crsync.go` + `server/cmd/server/router.go`（1 处 AIFIRST 挂载：`POST /api/daemon/cr-events`） | CR 投影 worker：事件幂等入账（`ON CONFLICT DO NOTHING`）→ per-CR 互斥 → 合法转移校验（transitions_gen）→ 更新 cr 行 / 乱序置 needs_reconcile → `cr:updated` 经 events.Bus 自动广播 workspace 房间。workspace 绑定只信 DaemonAuth 上下文，请求体 workspace_root_hash 仅日志用 | P1 §A 同步协议服务端半边；直接用 pgx 不走 sqlc（避免动上游 query 文件）。CR-2026-002 TASK-05 | 2026-07-31 |

## 纯配置约定（无代码改动，部署时执行）

| # | 项 | 口径 | 原因 |
|---|---|---|---|
| C1 | `MULTICA_CLOUD_FLEET_URL` / `MULTICA_FLEET_URL` | 永远留空 | 空值 → CloudPATVerifier 为 nil → `mcn_` 云节点令牌天然 401（`server/internal/middleware/auth.go`），本地执行路线不用云节点 |
| C2 | `DISABLE_WORKSPACE_CREATION` | 先建组织 workspace，随后置 `true` 并重启后端 | 单组织内部部署；上游 `.env.example` 自带此两阶段引导（#3433） |
| C3 | `ALLOWED_EMAIL_DOMAINS` | 部署时填公司邮箱域 | 限制内网注册来源；与 `ALLOW_SIGNUP` 组合为 AND 语义 |
| C4 | `.env` 行尾必须全程 LF | Windows 上不要用会改写行尾的工具碰 `.env` | 实测踩坑（2026-07-30）：`.env.example` 为 CRLF，`make selfhost` 复制生成的 `.env` 带 `\r`，Postgres 卷用"密码+`\r`"初始化；之后任何把行尾规范成 LF 的编辑（如 Git Bash 的 `sed -i`）都会造成 backend 与已初始化卷的密码不一致（SASL auth failed）。修复方式：数据可弃时 `docker-compose down -v` 重建卷 |
| C5 | 本机 agent CLI 需已独立 `claude /login`（daemon 干净环境要求已根修，见代码改动 #2） | 实测踩坑（2026-07-31，CR-2026-001 TASK-04）：Claude Code 桌面 App 的登录态不共享给命令行 `claude`，须一次性 `/login` | 同左 |

## 已知测试失败基线（上游既有，非本 fork 引入）

双周 rebase 后跑全量测试时，以下失败为已知基线，不计入"本次改动引入的回归"；若数量或名单变化才需要排查：

| 测试 | 包 | 确认方式 | 记录日期 |
|---|---|---|---|
| `TestTraecliBlockedArgsFiltering` / `TestQoderBackendInvokesACPFlagAndFiltersBlockedArgs` / `TestQoderFiltersRemoteMcpWhenInitializeDoesNotAdvertiseCapability` | `server/pkg/agent` | `git stash` 摘除本地全部改动后仍失败（2026-07-31，Windows + 本机装有 Qoder 的环境）；根因未诊断，疑与测试对本机环境的隐含假设有关 | 2026-07-31 |
| `TestNewAPIClient_LeftoverMarkerActionableError` 等 7 项 | `server/cmd/multica` | 未改动的 main 检出 A/B 复跑结果完全一致（2026-07-31，CR-2026-002 TASK-05 全量基线时发现）；多为 Windows 路径分隔符/本机环境假设 | 2026-07-31 |
| `TestCLIConfig_BackwardCompat_*` 等 4 项 | `server/internal/cli` | 同上 A/B 验证一致 | 2026-07-31 |
| gofmt：本机 Go 工具链对上游 794 个文件报格式差异 | 全仓 | 上游格式化用的 Go 版本与本机不同；**本 fork 新增文件必须过本机 gofmt**，上游文件不动 | 2026-07-31 |

## 未做（防止误以为已做）

- controlled-shell/gitguard 下沉 daemon（P1-F5，M0 明确范围外——当前 Agent 执行未受白名单约束）
- CR 投影表 / 签名审批 / Pipeline Runner 等 P1 schema 与服务（P0 映射表 §3，随 P1 的 CR 落地）
