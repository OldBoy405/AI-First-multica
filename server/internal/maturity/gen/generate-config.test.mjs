import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { test, after } from 'node:test';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const GEN = join(__dirname, 'generate-config.mjs');
const OUT = join(__dirname, '..', 'config_gen.go');

// The tests overwrite the committed generated file; restore it afterwards so
// the suite leaves a clean working tree (CI safety).
const ORIGINAL_OUT = existsSync(OUT) ? readFileSync(OUT, 'utf8') : null;
after(() => {
  if (ORIGINAL_OUT === null) rmSync(OUT, { force: true });
  else writeFileSync(OUT, ORIGINAL_OUT);
});

const VALID = `# test config
schema: ai-first.maturity-config/v1
observation_weeks: 4
calibration_status: observing
dimensions:
  AIF: [token_intensity, ai_penetration]
  SII: [cr_throughput_per_capita]
  OFI: [project_collab_scale, project_active_rate]
  EPC: [prototype_direct_rate]
  ACM: [team_agent_depth, process_completion_rate]
metrics:
  token_intensity: {weight: 0.125, floor: 0, target: 1}
  ai_penetration: {weight: 0.125, floor: 0, target: 1}
  cr_throughput_per_capita: {weight: 0.125, floor: 0, target: 1}
  project_collab_scale: {weight: 0.125, floor: 0, target: 1}
  project_active_rate: {weight: 0.125, floor: 0, target: 1}
  prototype_direct_rate: {weight: 0.125, floor: 0, target: 1}
  team_agent_depth: {weight: 0.125, floor: 0, target: 1}
  process_completion_rate: {weight: 0.125, floor: 0, target: 1}
`;

function fixture(t, configText = VALID, extraFiles = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'gencfg-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  writeFileSync(join(dir, 'maturity-config.yaml'), configText);
  for (const [name, content] of Object.entries(extraFiles)) {
    mkdirSync(dirname(join(dir, name)), { recursive: true });
    writeFileSync(join(dir, name), content);
  }
  execFileSync('git', ['init', '-q'], { cwd: dir });
  execFileSync('git', ['-c', 'user.email=t@t', '-c', 'user.name=t', 'add', '-A'], { cwd: dir });
  execFileSync('git', ['-c', 'user.email=t@t', '-c', 'user.name=t', 'commit', '-qm', 'seed'], { cwd: dir });
  return dir;
}

function run(args, cwd) {
  try {
    return { ok: true, stdout: execFileSync('node', [GEN, ...args], { cwd, encoding: 'utf8' }) };
  } catch (e) {
    return { ok: false, code: e.status, stdout: e.stdout ?? '', stderr: e.stderr ?? '' };
  }
}

test('generates gofmt-clean output and --check agrees', (t) => {
  const dir = fixture(t);
  const r = run(['--source', dir], dir);
  assert.ok(r.ok, r.stderr);
  const gen = readFileSync(OUT, 'utf8');
  assert.match(gen, /Source: knowledge-base@[0-9a-f]{40} maturity-config\.yaml/);
  assert.match(gen, /CalibrationStatus: "observing"/);
  assert.match(gen, /MetricTokenIntensity:\s+\{Weight: 0\.125, Floor: 0, Target: 1\}/);
  assert.match(gen, /func GeneratedPriceMap\(\) \(PriceMap, bool\) \{ return PriceMap\{\}, false \}/);
  const c = run(['--source', dir, '--check'], dir);
  assert.ok(c.ok, c.stderr);
});

test('LF and CRLF sources produce byte-identical output', (t) => {
  const lf = fixture(t, VALID);
  run(['--source', lf], lf);
  const a = readFileSync(OUT, 'utf8');
  const crlf = fixture(t, VALID.replaceAll('\n', '\r\n'));
  run(['--source', crlf], crlf);
  const b = readFileSync(OUT, 'utf8');
  assert.equal(a, b);
});

test('missing metrics entry hard-fails naming the block', (t) => {
  const dir = fixture(t, VALID.replace('  process_completion_rate: {weight: 0.125, floor: 0, target: 1}\n', ''));
  const r = run(['--source', dir], dir);
  assert.equal(r.ok, false);
  assert.match(r.stderr, /process_completion_rate/);
});

test('weight sum != 1 hard-fails', (t) => {
  const dir = fixture(t, VALID.replace('token_intensity: {weight: 0.125', 'token_intensity: {weight: 0.5'));
  const r = run(['--source', dir], dir);
  assert.equal(r.ok, false);
  assert.match(r.stderr, /sum to 1/);
});

test('unrecognized line hard-fails with line number', (t) => {
  const dir = fixture(t, VALID + 'banana: yes\n');
  const r = run(['--source', dir], dir);
  assert.equal(r.ok, false);
  assert.match(r.stderr, /unrecognized line/);
});

test('dirty source refuses generation', (t) => {
  const dir = fixture(t);
  writeFileSync(join(dir, 'maturity-config.yaml'), VALID.replace('observing', 'calibrated'));
  const r = run(['--source', dir], dir);
  assert.equal(r.ok, false);
  assert.match(r.stderr, /dirty\/untracked/);
});

test('--check fails on drifted committed file', (t) => {
  const dir = fixture(t);
  run(['--source', dir], dir);
  writeFileSync(OUT, readFileSync(OUT, 'utf8') + '\n// drift\n');
  const r = run(['--source', dir, '--check'], dir);
  assert.equal(r.ok, false);
  assert.match(r.stderr, /differs from a fresh regeneration/);
});

test('optional model-prices.yaml emits price map literal', (t) => {
  const dir = fixture(t, VALID, {
    'model-prices.yaml': `schema: ai-first.model-prices/v1
models:
  gpt-5.6: {input: 2.5, output: 10, cache_read: 0.25, cache_write: 2.5}
`,
  });
  const r = run(['--source', dir], dir);
  assert.ok(r.ok, r.stderr);
  const gen = readFileSync(OUT, 'utf8');
  assert.match(gen, /"gpt-5\.6": \{InputUSDPer1M: 2\.5/);
});

test('model-prices.yaml missing schema hard-fails', (t) => {
  const dir = fixture(t, VALID, { 'model-prices.yaml': 'models:\n  x: {input: 1, output: 1, cache_read: 1, cache_write: 1}\n' });
  const r = run(['--source', dir], dir);
  assert.equal(r.ok, false);
  assert.match(r.stderr, /schema/);
});
