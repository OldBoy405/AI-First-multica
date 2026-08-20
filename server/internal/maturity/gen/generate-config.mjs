#!/usr/bin/env node
// AIFIRST: generator for config_gen.go (CR-2026-047 TASK-01).
//
// Reads the knowledge-base authority files maturity-config.yaml and optional
// model-prices.yaml and emits a committed read-only Go copy used at runtime:
// server/internal/maturity/config_gen.go. The generated file records the
// last commit that changed maturity-config.yaml so unrelated KB commits never
// drift config_rev.
//
// The parser is line-based and zero-dependency: any line that is not a
// comment/blank or an exact expected pattern is a hard error (SDD §2.4
// forbids silent degradation). Line endings are normalized \r\n -> \n first
// (Windows autocrlf has already bitten this repo twice).
//
// The source file must be committed and clean relative to HEAD, otherwise the
// SHA embedded in the header would not match the content.
//
// Usage (from the multica repo root):
//   node server/internal/maturity/gen/generate-config.mjs [--source <dir>] [--check]
// Default source dir: the sibling worktree knowledge-base CR directory
// (../../../../../../../knowledge-base/requirement/CR-2026-047 relative to
// this file) — pass --source explicitly anywhere else.

import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const OUT = path.resolve(__dirname, '..', 'config_gen.go');

const SCHEMA = 'ai-first.maturity-config/v1';
const PRICE_SCHEMA = 'ai-first.model-prices/v1';

// Canonical ordering (SDD §2.4): any deviation in the source file is a hard error.
const DIMENSIONS = [
  ['AIF', ['token_intensity', 'ai_penetration']],
  ['SII', ['cr_throughput_per_capita']],
  ['OFI', ['project_collab_scale', 'project_active_rate']],
  ['EPC', ['prototype_direct_rate']],
  ['ACM', ['team_agent_depth', 'process_completion_rate']],
];
const METRIC_ORDER = DIMENSIONS.flatMap(([, ms]) => ms);
const GO_CONST = {
  token_intensity: 'MetricTokenIntensity',
  ai_penetration: 'MetricAIPenetration',
  cr_throughput_per_capita: 'MetricCRThroughputPerCapita',
  project_collab_scale: 'MetricProjectCollabScale',
  project_active_rate: 'MetricProjectActiveRate',
  prototype_direct_rate: 'MetricPrototypeDirectRate',
  team_agent_depth: 'MetricTeamAgentDepth',
  process_completion_rate: 'MetricProcessCompletionRate',
};

function fail(msg) {
  console.error(msg);
  process.exit(2);
}

function parseConfig(text) {
  const got = { schema: null, weeks: null, status: null, dims: {}, metrics: {} };
  const dimRe = /^  ([A-Z]{3}): \[([^\]]+)\]$/;
  const metricRe = /^  ([a-z_]+): \{weight: ([0-9.eE+-]+), floor: ([0-9.eE+-]+), target: ([0-9.eE+-]+)\}$/;
  const lines = text.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const t = line.trim();
    if (t === '' || t.startsWith('#')) continue;
    let m;
    if ((m = line.match(/^schema: (.+)$/))) got.schema = m[1].trim();
    else if ((m = line.match(/^observation_weeks: (\d+)$/))) got.weeks = Number(m[1]);
    else if ((m = line.match(/^calibration_status: ([a-z]+)$/))) got.status = m[1];
    else if (line === 'dimensions:' || line === 'metrics:') continue;
    else if ((m = line.match(dimRe))) got.dims[m[1]] = m[2].split(',').map((s) => s.trim());
    else if ((m = line.match(metricRe))) {
      got.metrics[m[1]] = { weight: Number(m[2]), floor: Number(m[3]), target: Number(m[4]) };
    } else {
      fail(`maturity-config.yaml:${i + 1}: unrecognized line: ${line}`);
    }
  }
  if (got.schema !== SCHEMA) fail(`maturity-config.yaml: missing/incorrect "schema" block (want ${SCHEMA})`);
  if (got.weeks !== 4) fail('maturity-config.yaml: missing/incorrect "observation_weeks" block (want 4)');
  if (got.status !== 'observing' && got.status !== 'calibrated') {
    fail('maturity-config.yaml: missing/incorrect "calibration_status" block (want observing|calibrated)');
  }
  for (const [dim, metrics] of DIMENSIONS) {
    const actual = got.dims[dim];
    if (!actual || actual.length !== metrics.length || actual.some((k, i) => k !== metrics[i])) {
      fail(`maturity-config.yaml: "dimensions" block mismatch for ${dim} (want [${metrics.join(', ')}])`);
    }
  }
  for (const key of METRIC_ORDER) {
    const mc = got.metrics[key];
    if (!mc) fail(`maturity-config.yaml: missing "metrics" entry ${key}`);
    if (!(mc.weight > 0 && mc.weight <= 1)) fail(`maturity-config.yaml: ${key} weight must be in (0,1]`);
    if (!(mc.target > mc.floor)) fail(`maturity-config.yaml: ${key} target must be > floor`);
  }
  const sum = METRIC_ORDER.reduce((a, k) => a + got.metrics[k].weight, 0);
  if (Math.abs(sum - 1) > 1e-9) fail(`maturity-config.yaml: metric weights must sum to 1 (got ${sum})`);
  return got;
}

function parsePrices(text) {
  const got = { schema: null, models: {} };
  const re = /^  ([A-Za-z0-9._:/+-]+): \{input: ([0-9.eE+-]+), output: ([0-9.eE+-]+), cache_read: ([0-9.eE+-]+), cache_write: ([0-9.eE+-]+)\}$/;
  const lines = text.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const t = line.trim();
    if (t === '' || t.startsWith('#')) continue;
    let m;
    if ((m = line.match(/^schema: (.+)$/))) got.schema = m[1].trim();
    else if (line === 'models:') continue;
    else if ((m = line.match(re))) {
      got.models[m[1]] = { input: Number(m[2]), output: Number(m[3]), cacheRead: Number(m[4]), cacheWrite: Number(m[5]) };
    } else {
      fail(`model-prices.yaml:${i + 1}: unrecognized line: ${line}`);
    }
  }
  if (got.schema !== PRICE_SCHEMA) fail('model-prices.yaml: missing/incorrect "schema" block');
  return got;
}

function emitGo(config, prices, sourceSha) {
  const dimLines = DIMENSIONS.map(
    ([d, ms]) => `\t\tDim${d}: {${ms.map((k) => GO_CONST[k]).join(', ')}},`,
  );
  const metricLines = METRIC_ORDER.map((k) => {
    const m = config.metrics[k];
    return `\t\t${GO_CONST[k]}: {Weight: ${m.weight}, Floor: ${m.floor}, Target: ${m.target}},`;
  });
  let priceFn;
  if (prices) {
    const priceLines = Object.entries(prices.models)
      .sort(([a], [b]) => (a < b ? -1 : 1))
      .map(
        ([model, p]) =>
          `\t\t${JSON.stringify(model)}: {InputUSDPer1M: ${p.input}, OutputUSDPer1M: ${p.output}, CacheReadUSDPer1M: ${p.cacheRead}, CacheWriteUSDPer1M: ${p.cacheWrite}},`,
      );
    priceFn = [
      'func GeneratedPriceMap() (PriceMap, bool) {',
      '\treturn PriceMap{Models: map[string]ModelPrice{',
      ...priceLines,
      '\t}}, true',
      '}',
    ].join('\n');
  } else {
    priceFn = 'func GeneratedPriceMap() (PriceMap, bool) { return PriceMap{}, false }';
  }
  return [
    '// Code generated by gen/generate-config.mjs from knowledge-base maturity-config.yaml. DO NOT EDIT.',
    '//',
    '// AIFIRST: committed read-only copy of the maturity config declaration (CR-2026-047 TASK-01).',
    `// Source: knowledge-base@${sourceSha} maturity-config.yaml`,
    '// Consistency is guarded by the gen script --check mode: regenerate != this file -> non-zero exit.',
    'package maturity',
    '',
    '// GeneratedConfigRev returns the commit that last changed maturity-config.yaml.',
    `func GeneratedConfigRev() string { return ${JSON.stringify(sourceSha)} }`,
    '',
    '// GeneratedConfig returns the committed copy of maturity-config.yaml.',
    'func GeneratedConfig() ConfigV1 {',
    '\treturn ConfigV1{',
    `\t\tSchema: ${JSON.stringify(SCHEMA)},`,
    `\t\tObservationWeeks: ${config.weeks},`,
    `\t\tCalibrationStatus: ${JSON.stringify(config.status)},`,
    '\t\tDimensions: map[DimensionKey][]MetricKey{',
    ...dimLines,
    '\t\t},',
    '\t\tMetrics: map[MetricKey]MetricConfig{',
    ...metricLines,
    '\t\t},',
    '\t}',
    '}',
    '',
    `// GeneratedPriceMap returns the committed copy of the optional model-prices.yaml.
// The bool is false when no price map is declared.
${priceFn}`,
  ].join('\n');
}

function gofmt(text) {
  const r = spawnSync('gofmt', [], { input: text, encoding: 'utf8', shell: false });
  if (r.status !== 0) fail(`gofmt failed: ${r.stderr}`);
  return r.stdout;
}

function main() {
  const args = process.argv.slice(2);
  const check = args.includes('--check');
  const srcIdx = args.indexOf('--source');
  const sourceDir =
    srcIdx >= 0
      ? path.resolve(args[srcIdx + 1])
      : path.resolve(__dirname, '..', '..', '..', '..', '..', '..', '..', 'knowledge-base', 'requirement', 'CR-2026-047');
  if (!fs.existsSync(path.join(sourceDir, 'maturity-config.yaml'))) {
    fail(`source dir has no maturity-config.yaml: ${sourceDir} (pass --source <dir>)`);
  }
  const dirty = spawnSync('git', ['-C', sourceDir, 'status', '--porcelain', '--', 'maturity-config.yaml', 'model-prices.yaml'], {
    encoding: 'utf8',
    shell: false,
  });
  if (dirty.status !== 0) fail(`git status failed in ${sourceDir}: ${dirty.stderr}`);
  if (dirty.stdout.trim() !== '') fail(`source files are dirty/untracked relative to HEAD in ${sourceDir}; commit first`);
  const shaR = spawnSync('git', ['-C', sourceDir, 'log', '-1', '--format=%H', '--', 'maturity-config.yaml'], {
    encoding: 'utf8',
    shell: false,
  });
  const sourceSha = shaR.status === 0 ? shaR.stdout.trim() : fail('git log failed for maturity-config.yaml');
  if (!/^[0-9a-f]{40}$/.test(sourceSha)) fail(`source commit SHA is not 40-hex: ${sourceSha}`);

  const config = parseConfig(fs.readFileSync(path.join(sourceDir, 'maturity-config.yaml'), 'utf8').replaceAll('\r\n', '\n'));
  let prices = null;
  const pricePath = path.join(sourceDir, 'model-prices.yaml');
  if (fs.existsSync(pricePath)) {
    prices = parsePrices(fs.readFileSync(pricePath, 'utf8').replaceAll('\r\n', '\n'));
  }

  const out = gofmt(emitGo(config, prices, sourceSha));

  if (check) {
    const existing = fs.existsSync(OUT) ? fs.readFileSync(OUT, 'utf8').replaceAll('\r\n', '\n') : '';
    if (existing !== out.replaceAll('\r\n', '\n')) {
      console.error('config_gen.go differs from a fresh regeneration off maturity-config.yaml — regenerate and commit.');
      process.exit(1);
    }
    console.log(`config consistency OK (knowledge-base@${sourceSha.slice(0, 7)}${prices ? ', with price map' : ''})`);
  } else {
    fs.writeFileSync(OUT, out, 'utf8');
    console.log(`generated ${OUT} (source knowledge-base@${sourceSha.slice(0, 7)}${prices ? ', with price map' : ''})`);
  }
}

main();
