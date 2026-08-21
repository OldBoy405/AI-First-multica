#!/usr/bin/env node
// AIFIRST: CR-2026-049 TASK-08 — generator for config_gen.go.
//
// Reads the knowledge-base dir-graph.yaml#repositories[] declaration (id, path,
// trunk, role, remote, commit_prefixes, active, description) and emits a
// committed read-only Go copy: server/internal/commitprefix/config_gen.go.
// The generated file records the commit that last changed dir-graph.yaml so
// unrelated KB commits never drift the scan config_rev (same discipline as
// maturity/gen/generate-config.mjs).
//
// Parser is line-based and zero-dependency: any line inside the repositories
// block that is not a blank/comment or an exact expected pattern is a hard
// error (discipline #1: no silent degradation). Line endings are normalized
// \r\n -> \n first. The source file must be committed and clean relative to
// HEAD, otherwise the SHA embedded in the header would not match the content.
//
// Usage (from the multica repo root):
//   node server/internal/commitprefix/gen/generate-prefixes.mjs [--source <dir>] [--check]
// Default source dir: the sibling knowledge-base worktree
// (../../../../../../knowledge-base/requirement/CR-2026-049 relative to
// this file) — pass --source explicitly anywhere else.

import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const OUT = path.resolve(__dirname, '..', 'config_gen.go');

const EXPECTED_IDS = ['ai-first-platform-docs', 'multica', 'tools'];

function fail(msg) {
  console.error(msg);
  process.exit(2);
}

// Minimal strict flow-sequence parser for ["a", "b", ...] (double-quoted items only).
function parseFlowSeq(line, file, lineno) {
  const t = line.trim();
  if (!t.startsWith('[') || !t.endsWith(']')) fail(`${file}:${lineno}: commit_prefixes must be a flow sequence`);
  const inner = t.slice(1, -1);
  if (inner.trim() === '') return [];
  const out = [];
  let i = 0;
  while (i < inner.length) {
    while (i < inner.length && /\s/.test(inner[i])) i++;
    if (inner[i] !== '"') fail(`${file}:${lineno}: commit_prefixes items must be double-quoted`);
    let j = i + 1;
    while (j < inner.length && inner[j] !== '"') j++;
    if (j >= inner.length) fail(`${file}:${lineno}: unterminated string in commit_prefixes`);
    out.push(inner.slice(i + 1, j));
    i = j + 1;
    while (i < inner.length && /\s/.test(inner[i])) i++;
    if (i >= inner.length) break;
    if (inner[i] !== ',') fail(`${file}:${lineno}: expected ',' in commit_prefixes`);
    i++;
  }
  return out;
}

const KNOWN_FIELDS = new Set(['id', 'path', 'trunk', 'role', 'remote', 'commit_prefixes', 'active', 'description']);

function parseDirGraph(text, file) {
  const lines = text.split('\n');
  const repos = [];
  let cur = null;
  let inRepos = false;
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const t = line.trim();
    if (t === '' || t.startsWith('#')) continue;
    if (/^repositories:\s*$/.test(t)) {
      inRepos = true;
      continue;
    }
    if (inRepos && /^[A-Za-z_][A-Za-z0-9_]*:\s*/.test(line) && !line.startsWith(' ') && !line.startsWith('-')) {
      inRepos = false; // next top-level section
      continue;
    }
    if (!inRepos) continue;
    if (/^- id: (\S+)\s*$/.test(t)) {
      cur = { id: RegExp.$1 };
      repos.push(cur);
      continue;
    }
    if (!cur) fail(`${file}:${i + 1}: unexpected line outside a repository entry: ${line}`);
    const m = /^([a-z_]+):\s*(.*)$/.exec(t);
    if (!m) fail(`${file}:${i + 1}: unrecognized line in repositories block: ${line}`);
    const key = m[1];
    const val = m[2];
    if (!KNOWN_FIELDS.has(key)) fail(`${file}:${i + 1}: unknown repository field: ${key}`);
    if (key === 'commit_prefixes') cur.prefixes = parseFlowSeq(val, file, i + 1);
    else if (key === 'path' || key === 'remote' || key === 'trunk' || key === 'description') {
      cur[key] = val.replace(/^"(.*)"$/, '$1').trim();
    } else if (key === 'role') {
      cur.role = val.trim();
    } else if (key === 'active') {
      cur.active = val.trim() === 'true';
    }
  }
  return repos;
}

function validateRepos(repos, file) {
  if (repos.length !== EXPECTED_IDS.length) {
    fail(`${file}: expected ${EXPECTED_IDS.length} repositories, got ${repos.length}`);
  }
  for (const want of EXPECTED_IDS) {
    const r = repos.find((x) => x.id === want);
    if (!r) fail(`${file}: missing repository entry ${want}`);
    if (!r.trunk) fail(`${file}: ${want} missing trunk`);
    if (!r.remote) fail(`${file}: ${want} missing remote`);
    const m = /^https:\/\/github\.com\/([^/]+)\/([^/]+?)(?:\.git)?$/.exec(r.remote);
    if (!m) fail(`${file}: ${want} remote is not a GitHub HTTPS URL: ${r.remote}`);
    r.owner = m[1];
    r.repo = m[2];
    if (!Array.isArray(r.prefixes) || r.prefixes.length === 0) {
      fail(`${file}: ${want} commit_prefixes must be non-empty`);
    }
    for (const p of r.prefixes) {
      if (typeof p !== 'string' || p === '') fail(`${file}: ${want} has an empty prefix`);
      if (p === 'wip:' || p.startsWith('wip:')) {
        fail(`${file}: ${want} prefix ${JSON.stringify(p)} — 'wip:' is the reserved priority keyword and must not enter the whitelist`);
      }
    }
    const dup = new Set(r.prefixes);
    if (dup.size !== r.prefixes.length) fail(`${file}: ${want} has duplicate prefixes`);
  }
}

function emitGo(repos, sourceSha) {
  const entries = EXPECTED_IDS.map((id) => {
    const r = repos.find((x) => x.id === id);
    return [
      '\t\t' + JSON.stringify(r.id) + ': {',
      `\t\t\tID: ${JSON.stringify(r.id)},`,
      `\t\t\tCanonicalURL: ${JSON.stringify(r.remote)},`,
      `\t\t\tOwner: ${JSON.stringify(r.owner)},`,
      `\t\t\tRepo: ${JSON.stringify(r.repo)},`,
      `\t\t\tTrunk: ${JSON.stringify(r.trunk)},`,
      `\t\t\tPrefixes: []string{${r.prefixes.map((p) => JSON.stringify(p)).join(', ')}},`,
      '\t\t},',
    ].join('\n');
  });
  return [
    '// Code generated by gen/generate-prefixes.mjs from knowledge-base dir-graph.yaml. DO NOT EDIT.',
    '//',
    '// AIFIRST: committed read-only copy of the E5 commit-prefix declaration (CR-2026-049 TASK-08).',
    `// Source: knowledge-base@${sourceSha} dir-graph.yaml`,
    '// Consistency is guarded by the gen script --check mode: regenerate != this file -> non-zero exit.',
    'package commitprefix',
    '',
    '// generatedConfigRev returns the commit that last changed dir-graph.yaml.',
    `var generatedConfigRev = ${JSON.stringify(sourceSha)}`,
    '',
    'var generatedPrefixes = map[string]RepoPrefixDecl{',
    ...entries,
    '}',
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
      : path.resolve(__dirname, '..', '..', '..', '..', '..', '..', '..', '..', 'knowledge-base', 'requirement', 'CR-2026-049');
  const file = path.join(sourceDir, 'dir-graph.yaml');
  if (!fs.existsSync(file)) fail(`source dir has no dir-graph.yaml: ${sourceDir} (pass --source <dir>)`);
  const dirty = spawnSync('git', ['-C', sourceDir, 'status', '--porcelain', '--', 'dir-graph.yaml'], {
    encoding: 'utf8',
    shell: false,
  });
  if (dirty.status !== 0) fail(`git status failed in ${sourceDir}: ${dirty.stderr}`);
  if (dirty.stdout.trim() !== '') fail(`dir-graph.yaml is dirty/untracked relative to HEAD in ${sourceDir}; commit first`);
  const shaR = spawnSync('git', ['-C', sourceDir, 'log', '-1', '--format=%H', '--', 'dir-graph.yaml'], {
    encoding: 'utf8',
    shell: false,
  });
  const sourceSha = shaR.status === 0 ? shaR.stdout.trim() : fail('git log failed for dir-graph.yaml');
  if (!/^[0-9a-f]{40}$/.test(sourceSha)) fail(`source commit SHA is not 40-hex: ${sourceSha}`);

  const repos = parseDirGraph(fs.readFileSync(file, 'utf8').replaceAll('\r\n', '\n'), file);
  validateRepos(repos, file);

  const out = gofmt(emitGo(repos, sourceSha)) + '\n';
  if (check) {
    const existing = fs.existsSync(OUT) ? fs.readFileSync(OUT, 'utf8') : '';
    if (existing !== out) {
      fail('config_gen.go drift: regenerate with node server/internal/commitprefix/gen/generate-prefixes.mjs --source <dir>');
    }
    console.log('config_gen.go matches dir-graph.yaml declaration');
    return;
  }
  fs.writeFileSync(OUT, out, 'utf8');
  console.log(`wrote ${OUT} (source ${sourceSha})`);
}

main();
