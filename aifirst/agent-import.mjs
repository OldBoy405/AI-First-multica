#!/usr/bin/env node
// agent-import.mjs — register the tools package's active agents into Multica
// via POST /api/agents (CR-2026-001 TASK-03 / FR-2).
//
// Zero-dependency (node: builtins only). Contract facts verified in TASK-02:
//   - POST /api/agents requires name + runtime_id; description <= 255 code
//     points; instructions is the runtime prompt (description is catalog-only).
//   - The server has NO name-uniqueness constraint — idempotency is OUR job:
//     GET /api/agents first, skip names that already exist.
//   - frontmatter `mode` / `permission.bash` have no persisted home in the
//     agent row; they are logged (fieldsReadNotPersisted) and NOT silently
//     dropped. Enforcement of permission.bash is P1 gitguard scope.
//
// Usage:
//   MULTICA_TOKEN=mul_xxx node aifirst/agent-import.mjs \
//     --tools <path-to-tools-repo> --workspace <workspace-uuid> --runtime <runtime-uuid>
//   [--api http://localhost:8080]

import fs from 'node:fs';
import path from 'node:path';

function arg(name, def) {
  const i = process.argv.indexOf(`--${name}`);
  return i > -1 ? process.argv[i + 1] : def;
}

const API = arg('api', 'http://localhost:8080');
const TOOLS = arg('tools', null);
const WORKSPACE = arg('workspace', process.env.MULTICA_WORKSPACE_ID || null);
const RUNTIME = arg('runtime', null);
const TOKEN = process.env.MULTICA_TOKEN || '';

if (!TOOLS || !WORKSPACE || !RUNTIME || !TOKEN) {
  console.error('missing required input: --tools, --workspace, --runtime, and env MULTICA_TOKEN are all required');
  process.exit(1);
}

const HEADERS = {
  Authorization: `Bearer ${TOKEN}`,
  'X-Workspace-ID': WORKSPACE,
  'Content-Type': 'application/json',
};

// ── parse agents/_index.yml: active agent ids + doc paths (line-based) ──
function activeAgents() {
  const out = [];
  let cur = null;
  for (const line of fs.readFileSync(path.join(TOOLS, 'agents/_index.yml'), 'utf8').split('\n')) {
    const idM = line.match(/^\s{2}-\s*id:\s*(\S+)/);
    if (idM) { cur = { id: idM[1], path: null, status: null }; out.push(cur); continue; }
    if (!cur) continue;
    const pathM = line.match(/^\s{4}path:\s*(\S+)/);
    if (pathM) cur.path = pathM[1];
    const statusM = line.match(/^\s{4}status:\s*(\S+)/);
    if (statusM) cur.status = statusM[1];
  }
  return out.filter((a) => a.status === 'active');
}

// ── parse one agent .md: frontmatter (name/description/mode/permission.bash) + body ──
function parseAgentDoc(absPath) {
  const text = fs.readFileSync(absPath, 'utf8');
  const lines = text.split('\n');
  if (lines[0].trim() !== '---') throw new Error(`${absPath}: no frontmatter`);
  const fm = { name: null, description: null, mode: null, permissionBash: null };
  let bodyStart = 0;
  let inPermission = false;
  for (let i = 1; i < lines.length; i++) {
    const line = lines[i].replace(/\r$/, '');
    if (line.trim() === '---') { bodyStart = i + 1; break; }
    const kv = line.match(/^(\w[\w-]*):\s*(.*)$/);
    if (kv) {
      inPermission = kv[1] === 'permission' && kv[2] === '';
      if (kv[1] === 'name') fm.name = kv[2].trim();
      if (kv[1] === 'description') fm.description = kv[2].trim();
      if (kv[1] === 'mode') fm.mode = kv[2].trim();
      continue;
    }
    const nested = line.match(/^\s+bash:\s*(\S+)/);
    if (inPermission && nested) fm.permissionBash = nested[1];
  }
  const body = lines.slice(bodyStart).join('\n').trim();
  return { fm, body };
}

async function api(method, route, body) {
  const res = await fetch(`${API}${route}`, {
    method,
    headers: HEADERS,
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${method} ${route} -> ${res.status}: ${text.slice(0, 300)}`);
  return text ? JSON.parse(text) : null;
}

const existing = new Set((await api('GET', '/api/agents')).map((a) => a.name));
const agents = activeAgents();
let created = 0, skipped = 0, failed = 0;

for (const a of agents) {
  try {
    const { fm, body } = parseAgentDoc(path.join(TOOLS, 'agents', a.path.replace(/^\.\//, '')));
    if (!fm.name) throw new Error('frontmatter missing name');
    if ([...fm.description].length > 255) throw new Error(`description exceeds 255 code points (${[...fm.description].length})`);
    // SDD §2 verifiable discipline: read-but-not-persisted fields are logged, never silently dropped.
    console.log(JSON.stringify({ agent: fm.name, fieldsReadNotPersisted: { mode: fm.mode, 'permission.bash': fm.permissionBash } }));
    if (existing.has(fm.name)) {
      console.log(`skip: ${fm.name} (already registered)`);
      skipped++;
      continue;
    }
    const res = await api('POST', '/api/agents', {
      name: fm.name,
      description: fm.description || '',
      instructions: body,
      runtime_id: RUNTIME,
    });
    console.log(`created: ${fm.name} (${res.id})`);
    created++;
  } catch (e) {
    console.error(`failed: ${a.id} — ${e.message}`);
    failed++;
  }
}

console.log(`\nsummary: ${created} created, ${skipped} skipped, ${failed} failed (of ${agents.length} active agents)`);
// No process.exit(): on Windows it races undici's pending handles and aborts
// with a libuv assertion. Setting exitCode lets the loop drain and exit clean.
process.exitCode = failed > 0 ? 1 : 0;
