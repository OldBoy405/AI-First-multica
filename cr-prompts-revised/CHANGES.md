# CR 协调小组协作流程优化 — 改动说明（2026-09-15，第 2 版）

修订对象：Multica workspace `AI First Platform` 的 5 个 CR 流程 Agent Prompt + `CR 协调小组` squad instructions。
基线：各 Agent 当前线上 `instructions` 原文。本目录文件为**整份可直接复制**的替换版本。

第 2 版按「Agent 该拥有什么」的原则重做：**原位修订，零新增章节**——5 份 Prompt 的章节标题与线上基线**逐字一致**，新语义全部落在对应的既有章节里。

## 第 1 版的 4 类违规 → 第 2 版修法

| 违规 | 出处 | 修法 |
|---|---|---|
| **Git 算法进了 Prompt** | `delivery-agent` 新增的「归档后清理」六步命令序列（`merge-base --is-ancestor` → `branch -D` → `push origin --delete` → `ls-remote` 复核 → `workspace cleanup` → `fetch`+`pull --ff-only`） | 删掉命令序列。清理写成**职责+验收判据**：worktree/临时产物/trunk 同步归 `cr-archive` 与 `crctl workspace cleanup`（已有能力，只做逐仓核对）；分支删除标注**无 Skill 覆盖**，只在机器结果已证明合入 trunk 时按 `controlled-shell` 白名单既有形态经 `crctl git` 执行，证据不足报 `CLEANUP_UNCOVERED` 交人；并注明长期正解是把清理并入 `cr-archive`（tools 侧改动，需走 CR） |
| **多事实源** | ① reviewer 里的 stage→owner 映射表；② 审批命令被抄成 Prompt 常量；③ `requirement/{cr_id}` 分支名默认形态；④ controlled-shell 白名单细节与 `crctl.mjs` 行号；⑤ coordinator 承接信号时内联的 stage→Agent 映射 | ①②→「审批 stage 名、`--approver` 取值、命令形态**全部取自当前阶段 `approve-*` Skill`」，Prompt 只留占位；③→分支与路径取 merge/archive 返回值；④→「白名单本身是唯一事实源」；⑤→删除内联映射，改为「按路由表取责任 Agent」。coordinator 的事实源表新增两行（审批口径、人类 mention 标识），其余 Prompt 不再各自定义 |
| **重复 Skill 已有步骤** | reviewer「入口识别与证据」抄了 `review-code` 的证据清单（`test-report.md` 机器区、`test-evidence/cmd-NN.log`）、payload 文件名；reviewer/delivery 复述 `push-progress`、`bind-current-task`、`crctl merge/archive` 的参数 | 压成一句路由：「证据清单、读取顺序与取证范围以当前 review Skill 为准，本 Prompt 不复制清单」；payload 只说「该 Skill 指定的临时 payload（绝对路径）」；深原语表只留 Skill 名与原语名，不留参数 |
| **补丁式新增章节** | 第 1 版给 5 份 Prompt 各加了「输出风格」「上报边界」「PASS 收尾」「归档后清理」「人类审批信号承接」「汇报口径」等新标题 | 全部折回既有章节：输出风格 → 各 Prompt 既有的**完成标准**（coordinator 是**输出合同**）；上报边界 → 既有的**评审委派合同 / 独立评审合同**末条；PASS 审批收尾 → reviewer 既有的**发布职责（评审 PASS）**；归档清理 → delivery 既有的**输入与顺序**；人类信号承接 → coordinator 既有的**人工门禁** |

## 六项要求 → 落点（第 2 版）

| 要求 | 落点（均为既有章节） |
|---|---|
| 1 writer→reviewer 直连、异常才上报 | `requirement-writer`「定位」+「评审委派合同」末条；coordinator「路由」表质量门行 =「不介入」 |
| 2 全 PASS 后 @ 人 + PowerShell 审批指令；BLOCK 直接回修复评 | `quality-reviewer-agent`「发布职责与 PASS 收尾」「BLOCK 回修委派」 |
| 3 人类「审批通过」由 coordinator 承接 | `cr-coordinator-agent`「人工门禁」第 2 条（先核验 `crctl status`，再按 `crctl next` 单点委派） |
| 4 coordinator 不复述正常消息 | `cr-coordinator-agent`「输出合同」第 1–2 条 |
| 5 正常简洁、异常详细 | 5 份 Prompt 的「完成标准」/「输出合同」 |
| 6 归档后清理 | `delivery-agent`「输入与顺序」的归档后验收两段 + 「完成标准」 |

## 校验

- `lint-prompts.mjs --mode enforce`（新 Prompt 放入 tools 包副本 `agents/`）→ **0 findings**，exit 0。
- 章节标题与线上基线逐字比对：5 份均无新增/删除/改名。
- 体量：coordinator 4625→5697、requirement-writer 2852→3411、dev-agent 3389→4146、quality-reviewer 4942→6503、delivery-agent 2664→3367 字符。增量全部是新协作契约（审批指令模板、直连与上报判据、清理验收），无冗余步骤复制。

## 落地

CLI 只有 `--instructions`（无 `--instructions-file`），PowerShell：

```powershell
$d = "C:\Users\GOBAO\Downloads\AI\multica\cr-prompts-revised"
multica agent update 87ca2271-f4d8-4865-aef1-9a24523e1a20 --instructions (Get-Content -Raw "$d\cr-coordinator-agent.md")
multica agent update 6317495b-d913-4d47-be79-0c0b342b03fd --instructions (Get-Content -Raw "$d\requirement-writer.md")
multica agent update ff6fcbb6-6bb6-42fb-9d88-03493c771411 --instructions (Get-Content -Raw "$d\dev-agent.md")
multica agent update 2ed1a9de-4c8e-4b78-bfb1-055af99c6681 --instructions (Get-Content -Raw "$d\quality-reviewer-agent.md")
multica agent update cf0d9dac-93c7-4d51-bfd0-a86365b0142a --instructions (Get-Content -Raw "$d\delivery-agent.md")
multica squad update 364fcb42-b073-41be-9d21-f73acabdc7db --instructions (Get-Content -Raw "$d\squad-CR协调小组.md")
```

先更 5 个 Agent、再更 squad，避免出现「组规则要求的行为在 Agent Prompt 里还不存在」的空窗。

## 仍需你定的两件事

1. **分支清理的归属**：现在是 delivery-agent 在白名单内做（Prompt 里只有职责与判据，没有命令序列）。要不要改成给 `cr-archive` 增一个清理阶段（tools 侧 CR）？那样 Prompt 这段可以再瘦一层，只剩「核对 Skill 返回」。
2. **coordinator 纳入 lint 覆盖**：tools 仓 `agents/` 没有 `cr-coordinator-agent.md`，所以它的 Prompt 从来没被漂移检测扫过（线上现行版本就带 2 条 R1/R13 命中）。要不要把它补进 tools 仓的受扫范围（同样需走 CR）。
