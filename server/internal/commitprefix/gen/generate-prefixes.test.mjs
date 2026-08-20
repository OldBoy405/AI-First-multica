// AIFIRST: CR-2026-049 TASK-08 — commit_prefixes 声明与生成器测试。
// 覆盖：生成器对合法声明产出 config_gen.go（三仓、SDD §3.3 锁定值、wip: 保留字拒绝）；
// --check 漂移非零；非法声明行（未知字段/空前缀/wip: 前缀）硬失败；三仓 trunk 最近 200 条
// subject 的 coverage fixture（每项全匹配或显式归类为预期 finding，注释注明人工分类结论）。
import test from 'node:test';
import assert from 'node:assert';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const GEN = path.join(here, 'generate-prefixes.mjs');
const OUT = path.join(here, '..', 'config_gen.go');

const KB_WORKTREE = path.resolve(here, '..', '..', '..', '..', '..', '..', '..', 'knowledge-base', 'requirement', 'CR-2026-049');
// Coverage fixture 的仓库解析根（KB 主 checkout；worktree 布局下 ../multica 不是 multica 仓）。
const KB_MAIN = process.env.CR_049_COVERAGE_ROOT || 'C:/Users/GOBAO/Downloads/AI/AI First Platform';

function runGen(args, opts = {}) {
  return spawnSync(process.execPath, [GEN, ...args], { encoding: 'utf8', shell: false, ...opts });
}

test('TASK-08 AC-1：生成器产出三仓声明且与 SDD §3.3 锁定值一致', () => {
  assert.ok(fs.existsSync(path.join(KB_WORKTREE, 'dir-graph.yaml')), 'KB worktree 源存在（--source 覆盖可选）');
  const r = runGen(['--source', KB_WORKTREE, '--check']);
  assert.equal(r.status, 0, r.stderr);
  const out = fs.readFileSync(OUT, 'utf8');
  for (const id of ['ai-first-platform-docs', 'multica', 'tools']) {
    assert.ok(out.includes(`"${id}": {`), `${id} 条目缺失`);
  }
  // SDD §3.3 锁定前缀抽查 + wip: 不进入白名单
  assert.ok(out.includes('"[cr] "'));
  assert.ok(out.includes('"feat("'));
  assert.ok(out.includes('"MUL-"'));
  assert.ok(!out.includes('"wip:'), 'wip: 是优先分类保留字，禁止进入白名单');
  // 源 SHA 头
  assert.match(out, /Source: knowledge-base@[0-9a-f]{40} dir-graph\.yaml/);
});

function makeTmpSource(mutate) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'prefix-src-'));
  const graph = fs.readFileSync(path.join(KB_WORKTREE, 'dir-graph.yaml'), 'utf8');
  fs.writeFileSync(path.join(dir, 'dir-graph.yaml'), mutate(graph));
  spawnSync('git', ['-C', dir, 'init', '-q', '-b', 'master'], { encoding: 'utf8' });
  spawnSync('git', ['-C', dir, 'config', 'user.email', 'test@test'], { encoding: 'utf8' });
  spawnSync('git', ['-C', dir, 'config', 'user.name', 'test'], { encoding: 'utf8' });
  spawnSync('git', ['-C', dir, 'add', 'dir-graph.yaml'], { encoding: 'utf8' });
  spawnSync('git', ['-C', dir, 'commit', '-q', '-m', 'seed'], { encoding: 'utf8' });
  return dir;
}

test('TASK-08 AC-2：改声明后 --check 非零；非法声明行硬失败', () => {
  // 改一个前缀 → 现有 config_gen.go 与声明漂移 → --check 非零
  const d1 = makeTmpSource((s) => s.replace('"[cr] "', '"cr-go "'));
  const r1 = runGen(['--source', d1, '--check']);
  assert.notEqual(r1.status, 0, '--check 必须检测漂移');
  assert.match(r1.stderr, /drift/);
  fs.rmSync(d1, { recursive: true, force: true });

  // 未知字段
  const d2 = makeTmpSource((s) => s.replace('    trunk: main\n    role: code\n    remote:', '    trunk: main\n    role: code\n    bogus: 1\n    remote:'));
  const r2 = runGen(['--source', d2]);
  assert.notEqual(r2.status, 0);
  assert.match(r2.stderr, /unknown repository field/);
  fs.rmSync(d2, { recursive: true, force: true });

  // 空前缀
  const d3 = makeTmpSource((s) => s.replace('"[cr] ", "register "', '"", "register "'));
  const r3 = runGen(['--source', d3]);
  assert.notEqual(r3.status, 0);
  assert.match(r3.stderr, /empty prefix/);
  fs.rmSync(d3, { recursive: true, force: true });

  // wip: 前缀
  const d4 = makeTmpSource((s) => s.replace('"[cr] ", "register "', '"wip: ", "register "'));
  const r4 = runGen(['--source', d4]);
  assert.notEqual(r4.status, 0);
  assert.match(r4.stderr, /reserved priority keyword/);
  fs.rmSync(d4, { recursive: true, force: true });
});

// ── coverage fixture（AC-10）：三仓 trunk 最近 200 条 subject ────────────────
// 以下列表是 2026-08-20 人工分类结果：这些历史 subject 不符合白名单，按声明
// 口径是真实预期 finding（legacy 一次性提交 / 上游原生格式 / 早期 merge 文案），
// 不在本次 CR 范围修正；新增未匹配 subject 会令本测试失败，强制重新人工分类。
const EXPECTED_FINDINGS = {
  'ai-first-platform-docs': [
    'review(CR-2026-047): code PASS',
    'review(CR-2026-047): code attempt 3 BLOCK',
    'review(CR-2026-047): code attempt 2 BLOCK',
    'review(CR-2026-047): code attempt 1 BLOCK',
    'docs: 补提交遗留的 analysis 文档归档移动（7cc5bfb 未完成的一半）',
    'docs: reorganize product/analysis docs into done/partial buckets, update dir-graph multica remote note',
    'fix: _index.yml 投影修复 CR-2026-019~026 status=archived（archive-move 自举期归档缺 index 写入，以 _history.yml 回填 archived-at/writeback-spec-id）',
    'review(CR-2026-044): pass code review attempt 3 (post-merge)',
    'review(CR-2026-044): pass code review attempt 2',
    'review(CR-2026-044): block code review attempt 1',
  ],
  multica: [
    'chore: update pnpm lockfile',
    'Revert "feat(desktop): open Settings with Cmd/Ctrl+, (MUL-6233) (#7022)" (#7079)',
    '[MUL-6269] Use Redis for channel WebSocket leases (#7055)',
    '[MUL-6139] Expand Plugin skills and hosted MCP connections (#6920)',
    'docs: link the Multica CLI skill from the README and CLI docs (#6978)',
    'docs: explain the two local-directory execution modes (MUL-5707) (#6913)',
    '[MUL-6125] Ship Private Skill Plugin developer loop (#6900)',
    'fix: gate worktree mode on a declared capability, not a version string (MUL-5707) (#6904)',
    '[MUL-6099] Ship official Plugin V1 product vertical slice (#6869)',
  ],
  tools: [
    'merge(CR-2026-044): resolve tools trunk conflict with CR-2026-042 (README 90-line, code pipeline 16 nodes)',
    'implement CR-2026-040: structured test loop (testCr deep module)',
    'merge: integrate trunk (CR-2026-033 checkpoint deep primitive) into CR-2026-038 branch',
    'merge(CR-2026-029): 发布联调移交 merge pipeline 完成证据',
    'merge(CR-2026-028): tools 流程步骤优化 v2 前移优化项',
    'merge(CR-2026-027): tools 流程优化 Phase 0+1 — 基线事实统一与正确性修复（状态机口径 27/49、approve 原子提交、TASK 归档门禁、archive 原子化、终态查询、review-record 深化）',
    'merge(CR-2026-021): 治理工具链——prompt 对齐 crctl（S1~S11+inbox-emit + lint-prompts 漂移防线）',
    'merge(CR-2026-020): 治理工具链 — writeback 机械步骤固化为入库脚本（三脚本 + SKILL 改调 + pipeline/ARCHITECTURE 修订）',
  ],
};

function readDecls(sourceDir) {
  const text = fs.readFileSync(path.join(sourceDir, 'dir-graph.yaml'), 'utf8').replaceAll('\r\n', '\n');
  const repos = {};
  let cur = null;
  for (const line of text.split('\n')) {
    const t = line.trim();
    const mId = /^- id: (\S+)/.exec(t);
    if (mId) { cur = mId[1]; repos[cur] = { id: cur }; continue; }
    const mTr = /^trunk: (\S+)/.exec(t);
    if (mTr && cur) repos[cur].trunk = mTr[1];
    const mPath = /^path: "?([^"]+)"?/.exec(t);
    if (mPath && cur) repos[cur].path = mPath[1];
    const mPre = /^commit_prefixes: \[(.*)\]/.exec(t);
    if (mPre && cur) repos[cur].prefixes = [...mPre[1].matchAll(/"([^"]+)"/g)].map((x) => x[1]);
  }
  return repos;
}

test('TASK-08 AC-3：三仓 trunk 最近 200 subject coverage fixture', () => {
  const repos = readDecls(KB_WORKTREE);
  let covered = 0;
  for (const id of ['ai-first-platform-docs', 'multica', 'tools']) {
    const r = repos[id];
    assert.ok(r && r.trunk && r.prefixes, `${id} 声明缺失`);
    const repoDir = path.resolve(KB_MAIN, r.path);
    const log = spawnSync('git', ['-C', repoDir, 'log', '-200', '--format=%s', r.trunk], { encoding: 'utf8' });
    if (log.status !== 0) {
      console.log(`TASK-08 coverage: ${id} repo unavailable (${repoDir}) — skip`);
      continue;
    }
    const allowed = new Set(EXPECTED_FINDINGS[id] || []);
    const subjects = log.stdout.split('\n').filter(Boolean).map((s) => s.split('\n')[0]);
    const miss = [];
    for (const s of subjects) {
      if (s.startsWith('wip:')) continue; // 优先分类保留字：预期 finding，不匹配白名单
      if (r.prefixes.some((p) => s.startsWith(p))) continue;
      if (allowed.has(s)) continue;
      miss.push(s);
    }
    if (miss.length) {
      assert.fail(`${id} trunk 最近 200 subject 有 ${miss.length} 条未分类：\n  ` + miss.join('\n  ') + '\n请人工分类：加入白名单（改声明）或显式列入 EXPECTED_FINDINGS');
    }
    covered++;
  }
  assert.ok(covered > 0, '至少一个仓完成 coverage 校验');
});
