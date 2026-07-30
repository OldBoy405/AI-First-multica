# AI-First Fork 隔离约定

本文件不替代 [`CONTRIBUTING.md`](CONTRIBUTING.md)——那份是上游的本地开发指南（环境搭建/worktree/测试），照旧遵守。本文件只管一件事：**我们在这个 fork 上加代码时，怎么加才能让跟上游（`multica-ai/multica`）的长期同步保持低成本**。

## 背景

本仓库是 AI-First 平台的 fork 基座，会长期持续同步上游。四条规则的目的是把"同步"变成一件双周例行小事，而不是攒到季度才做一次痛苦的大合并。

## 规则一：自研代码放新包，不散布进上游文件

- 后端新业务逻辑放新目录，例如 `server/internal/governance/`（CR 治理接入）、`server/internal/collab/`（三模式聊天增量）——禁止直接写进上游已有文件，尤其是变更频繁的大文件（`internal/daemon/daemon.go`、`internal/handler/daemon.go` 这类）。
- 前端同理：新页面/组件放独立目录（如 `apps/web/features/aifirst/`），不要散落进上游既有的 `features/` 子目录里跟原有代码混排。
- 判断标准：一个文件里如果同时有上游原有函数和我们新加的函数，就是违反了这条规则，应该拆出去。

## 规则二：改上游文件只做"挂钩点"，且必须标记

- 万不得已要改动上游文件（插入一次事件订阅、注册一个 middleware、读一个 feature flag）时，改动收敛到最少行数，且每处都留 `// AIFIRST: <一句话原因>` 注释。
- 优先复用上游已有的 `server/internal/featureflags/` 包做开关，不要自己发明一套配置机制。
- rebase 前先 `grep -rn "// AIFIRST:" server/ apps/` 过一遍，确认这些挂钩点在新的上游代码里还成立。

## 规则三：双周 rebase 例行化

- 每两周从 upstream `main` rebase 一次并跑全量测试——攒得越久，一次性冲突越大。
- 每次 rebase 后先看 `// AIFIRST:` 标记有没有冲突/失效，再看规则一的新建目录是否被上游误碰。
- rebase 完成后在 commit/PR 里注明 `synced with upstream @<SHA>`，方便追溯上一次同步点。

## 规则四：优先复用上游已有抽象，不重复造轮子

- 上游 `server/pkg/agent/` 已原生支持 `openclaw` 这个 Agent CLI 类型（含测试文件）——OpenClaw 渠道接入直接用这个抽象，不要另起一套。
- 一般原则：动手写新抽象前，先确认上游 `pkg/`、`internal/` 下是否已有等价能力，避免"重新发明 + 两套并存"的双倍维护成本。

## 违反规则怎么办

这不走 CR 治理那套门禁（那是给平台产品需求变更用的，这份约定管的是"我们怎么改这个 fork"）——code review 人工把关：PR 把自研逻辑塞进了上游文件、或者改了上游文件却没留 `// AIFIRST:` 标记，直接打回重做。
