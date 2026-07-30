# CUSTOM.md — AI-First fork 二开台账

逐条记录本 fork 相对上游（`multica-ai/multica`）的所有定制。每次双周 rebase 前先核对本表（配合 `grep -rn "// AIFIRST:" server/ apps/`，见 [CONTRIBUTING.AIFIRST.md](CONTRIBUTING.AIFIRST.md) 规则二、三）。

## 代码改动

| # | 位置 | 改动 | 原因 / 追溯 | 日期 |
|---|---|---|---|---|
| 1 | `server/cmd/server/router.go`（2 处，均有 `// AIFIRST:` 标记） | 摘除 Stripe webhook（`/api/webhooks/stripe`）与 `/api/cloud-billing` 路由组的挂载；handler 代码保留未删 | 内网自托管无云端计费；访问即 404。CR-2026-001 FR-1（AI First Platform 仓库 `change-requests/CR-2026-001/`） | 2026-07-30 |

## 纯配置约定（无代码改动，部署时执行）

| # | 项 | 口径 | 原因 |
|---|---|---|---|
| C1 | `MULTICA_CLOUD_FLEET_URL` / `MULTICA_FLEET_URL` | 永远留空 | 空值 → CloudPATVerifier 为 nil → `mcn_` 云节点令牌天然 401（`server/internal/middleware/auth.go`），本地执行路线不用云节点 |
| C2 | `DISABLE_WORKSPACE_CREATION` | 先建组织 workspace，随后置 `true` 并重启后端 | 单组织内部部署；上游 `.env.example` 自带此两阶段引导（#3433） |
| C3 | `ALLOWED_EMAIL_DOMAINS` | 部署时填公司邮箱域 | 限制内网注册来源；与 `ALLOW_SIGNUP` 组合为 AND 语义 |
| C4 | `.env` 行尾必须全程 LF | Windows 上不要用会改写行尾的工具碰 `.env` | 实测踩坑（2026-07-30）：`.env.example` 为 CRLF，`make selfhost` 复制生成的 `.env` 带 `\r`，Postgres 卷用"密码+`\r`"初始化；之后任何把行尾规范成 LF 的编辑（如 Git Bash 的 `sed -i`）都会造成 backend 与已初始化卷的密码不一致（SASL auth failed）。修复方式：数据可弃时 `docker-compose down -v` 重建卷 |
| C5 | daemon 必须从干净环境启动（先 unset `CLAUDE*`/`ANTHROPIC*`），且本机 agent CLI 需已独立 `claude /login` | 实测踩坑（2026-07-31，CR-2026-001 TASK-04）：从 Claude Code 会话内启动 daemon 时，`CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST` / `CLAUDE_CODE_HOST_AUTH_ENV_VAR` / `CLAUDE_CODE_CHILD_SESSION` 等宿主标记不在上游 `pkg/agent/claude.go#isFilteredChildEnvKey` 过滤名单里，泄漏给 claude 子进程后其误判认证归宿主管理，报 `Not logged in`。上游补全过滤名单是潜在回馈 PR | 同左 |

## 未做（防止误以为已做）

- controlled-shell/gitguard 下沉 daemon（P1-F5，M0 明确范围外——当前 Agent 执行未受白名单约束）
- CR 投影表 / 签名审批 / Pipeline Runner 等 P1 schema 与服务（P0 映射表 §3，随 P1 的 CR 落地）
