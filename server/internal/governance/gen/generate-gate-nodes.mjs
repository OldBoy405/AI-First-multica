#!/usr/bin/env node
// AIFIRST: generator for gate_nodes_gen.go (CR-2026-011 TASK-02).
//
// Reads the tools package's pipeline-templates/*.pipeline.json node arrays and
// emits a read-only Go copy of the node UUIDs the gate-node projector needs:
// the human_approval node for each of gates.json#approvalStages' four keys
// (requirement/tech-design/dev-start/code), and the review skill node that
// precedes each AI-reviewed one (requirement/tech-design/code — dev-start has
// no preceding AI review; see SDD DD-3).
//
// SDD TASK-02/TSUG-002 asked whether pipeline templates carry stable node
// UUIDs before committing to a derivation scheme (e.g. UUIDv5). They do — every
// node in every template already has a fixed `id` — so this generator just
// extracts them rather than deriving anything. That IS the "single decision
// point" TSUG-002 asked for: one generated file, one regeneration script,
// both sides (this and any future Runner) read the same constants.
//
// The generated file is COMMITTED (header records the source tools commit SHA).
// The --check mode regenerates and diffs against the committed file, exiting
// non-zero on drift — CI/pre-commit use it to guard consistency, so building
// multica never requires a tools checkout (mirrors generate-transitions.mjs).
//
// Usage (from the multica repo root):
//   node server/internal/governance/gen/generate-gate-nodes.mjs [--tools <path>] [--check]
// Default tools path: ../tools (sibling directory convention).

import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const OUT = path.resolve(__dirname, '..', 'gate_nodes_gen.go');

const args = process.argv.slice(2);
const check = args.includes('--check');
const toolsIdx = args.indexOf('--tools');
const toolsDir = toolsIdx >= 0 ? path.resolve(args[toolsIdx + 1]) : path.resolve(__dirname, '..', '..', '..', '..', '..', 'tools');

// stage key -> { pipeline template file (without extension), approval node ref
// is always null/human_approval so we locate it by kind, review node ref is
// the skill id that immediately precedes it in nodes[] }.
const STAGES = [
  { stage: 'requirement', pipeline: 'requirement-authoring', reviewRef: 'review-requirement' },
  { stage: 'tech-design', pipeline: 'architecture-design', reviewRef: 'review-tech-design' },
  { stage: 'dev-start', pipeline: 'code-implementation', reviewRef: null },
  { stage: 'code', pipeline: 'code-implementation', reviewRef: 'review-code' },
];

function loadTemplate(pipelineId) {
  const p = path.join(toolsDir, 'pipeline-templates', `${pipelineId}.pipeline.json`);
  const text = fs.readFileSync(p, 'utf8').replaceAll('\r\n', '\n');
  return JSON.parse(text);
}

const approvalNodes = {};
const reviewNodes = {};
const templateCache = {};

for (const { stage, pipeline, reviewRef } of STAGES) {
  const tmpl = templateCache[pipeline] ??= loadTemplate(pipeline);
  const nodes = tmpl.nodes;

  if (reviewRef) {
    const reviewIdx = nodes.findIndex((n) => n.ref === reviewRef);
    if (reviewIdx < 0) { console.error(`review node "${reviewRef}" not found in ${pipeline}.pipeline.json`); process.exit(2); }
    reviewNodes[stage] = { pipelineId: pipeline, nodeId: nodes[reviewIdx].id, seq: reviewIdx + 1, kind: nodes[reviewIdx].kind };
    // The human_approval node for a reviewed stage is the first human_approval
    // node AFTER the review node (there may be more than one human_approval in
    // a template — code-implementation has two: dev-start and code).
    const approvalIdx = nodes.findIndex((n, i) => i > reviewIdx && n.kind === 'human_approval');
    if (approvalIdx < 0) { console.error(`no human_approval node after "${reviewRef}" in ${pipeline}.pipeline.json`); process.exit(2); }
    approvalNodes[stage] = { pipelineId: pipeline, nodeId: nodes[approvalIdx].id, seq: approvalIdx + 1, kind: nodes[approvalIdx].kind };
  } else {
    // dev-start: the first human_approval node in the template, full stop.
    const approvalIdx = nodes.findIndex((n) => n.kind === 'human_approval');
    if (approvalIdx < 0) { console.error(`no human_approval node in ${pipeline}.pipeline.json`); process.exit(2); }
    approvalNodes[stage] = { pipelineId: pipeline, nodeId: nodes[approvalIdx].id, seq: approvalIdx + 1, kind: nodes[approvalIdx].kind };
  }
}

// CR-2026-045: compile the architecture-design Core registry from tools. The
// emitted registry carries the full node/prompt/permissions/replayLoop contract
// plus a canonical digest; both are embedded as generated Go constants so the
// Runner never reads a tools checkout at runtime.
const emitRegistry = path.join(toolsDir, 'pipeline-templates', 'emit-registry.mjs');
const regR = spawnSync(process.execPath, [emitRegistry, '--pipeline', 'architecture-design'], { encoding: 'utf8', shell: false });
if (regR.status !== 0) {
  console.error(`emit-registry failed:\n${regR.stderr}`);
  process.exit(2);
}
const registryRaw = regR.stdout.trim();
let registry;
try { registry = JSON.parse(registryRaw); } catch (e) { console.error(`emit-registry output not JSON: ${e.message}`); process.exit(2); }
if (!registry.digest || !/^sha256:[0-9a-f]{64}$/.test(registry.digest)) { console.error('emit-registry missing canonical digest'); process.exit(2); }

// feature-writeback carries no gate nodes we project (§4.1: entering it only
// opens/closes a pipeline_run row), but the projector needs its pipeline id
// as a plain string constant alongside the other three template ids.
const PIPELINE_IDS = ['requirement-authoring', 'architecture-design', 'code-implementation', 'feature-writeback'];

const shaR = spawnSync('git', ['-C', toolsDir, 'rev-parse', 'HEAD'], { encoding: 'utf8', shell: false });
const toolsSha = shaR.status === 0 ? shaR.stdout.trim() : 'unknown';

const lines = [];
lines.push('// Code generated by gen/generate-gate-nodes.mjs from tools pipeline-templates/*.pipeline.json. DO NOT EDIT.');
lines.push('//');
lines.push('// AIFIRST: read-only copy of the governance-relevant pipeline node UUIDs (CR-2026-011 TASK-02).');
lines.push(`// Source: tools@${toolsSha} pipeline-templates/{requirement-authoring,architecture-design,code-implementation}.pipeline.json`);
lines.push('// Consistency is guarded by the gen script --check mode: regenerate != this file -> non-zero exit.');
lines.push('package governance');
lines.push('');
lines.push('// PipelineIDs enumerates the four CR-scoped pipeline templates (in dependency order).');
lines.push('var PipelineIDs = struct {');
lines.push('\tRequirementAuthoring string');
lines.push('\tArchitectureDesign   string');
lines.push('\tCodeImplementation   string');
lines.push('\tFeatureWriteback     string');
lines.push('}{');
lines.push(`\tRequirementAuthoring: ${JSON.stringify(PIPELINE_IDS[0])},`);
lines.push(`\tArchitectureDesign:   ${JSON.stringify(PIPELINE_IDS[1])},`);
lines.push(`\tCodeImplementation:   ${JSON.stringify(PIPELINE_IDS[2])},`);
lines.push(`\tFeatureWriteback:     ${JSON.stringify(PIPELINE_IDS[3])},`);
lines.push('}');
lines.push('');
lines.push('// GateNode identifies one governance-relevant node in a pipeline template by');
lines.push('// its stable node UUID. The UUID is a template-authored constant (every node');
lines.push('// in every *.pipeline.json carries a fixed id) — this is the single decision');
lines.push('// point for node identity (SDD TASK-02/TSUG-002): both the projector here and');
lines.push('// any future Runner (CR-H) read the same generated constants, so the same');
lines.push('// logical node can never be projected under two different node_id values.');
lines.push('type GateNode struct {');
lines.push('\tPipelineID string');
lines.push('\tNodeID     string // UUID string form; stored verbatim into pipeline_node_run.node_id');
lines.push('\tSeq        int    // 1-indexed position in the template\'s nodes[] array');
lines.push('\tKind       string // matches pipeline_node_run.kind');
lines.push('}');
lines.push('');
lines.push('// ApprovalGateNodes maps a gates.json#approvalStages key (requirement/tech-design/');
lines.push('// dev-start/code) to its human_approval node.');
lines.push('var ApprovalGateNodes = map[string]GateNode{');
for (const { stage } of STAGES) {
  const n = approvalNodes[stage];
  lines.push(`\t${JSON.stringify(stage)}: {PipelineID: ${JSON.stringify(n.pipelineId)}, NodeID: ${JSON.stringify(n.nodeId)}, Seq: ${n.seq}, Kind: ${JSON.stringify(n.kind)}},`);
}
lines.push('}');
lines.push('');
lines.push('// ReviewGateNodes maps a review-event stage key (TASK-03\'s three scanned');
lines.push('// stages: requirement/tech-design/code — dev-start has no preceding AI review)');
lines.push('// to the review skill node whose blocked/passed outcome the review event');
lines.push('// channel projects.');
lines.push('var ReviewGateNodes = map[string]GateNode{');
for (const { stage, reviewRef } of STAGES) {
  if (!reviewRef) continue;
  const n = reviewNodes[stage];
  lines.push(`\t${JSON.stringify(stage)}: {PipelineID: ${JSON.stringify(n.pipelineId)}, NodeID: ${JSON.stringify(n.nodeId)}, Seq: ${n.seq}, Kind: ${JSON.stringify(n.kind)}},`);
}
lines.push('}');
lines.push('');
lines.push('// ArchitectureCoreRegistryJSON is the compiled architecture-design Core registry');
lines.push('// emitted by tools/pipeline-templates/emit-registry.mjs (CR-2026-045). It carries');
lines.push('// the full node/prompt/permissions/replayLoop contract; the Runner consumes this');
lines.push('// snapshot at runtime instead of reading a tools checkout.');
lines.push('var ArchitectureCoreRegistryJSON = ' + JSON.stringify(registryRaw));
lines.push('');
lines.push('// ArchitectureCoreRegistryDigest is the canonical SHA-256 of the registry body');
lines.push('// (pipeline + pipelineOwner + nodePermissions). Runner recovery compares it');
lines.push('// against pipeline_run.execution_context.template_digest and fails closed on drift.');
lines.push('const ArchitectureCoreRegistryDigest = ' + JSON.stringify(registry.digest));
lines.push('');

function gofmt(source) {
  const r = spawnSync('gofmt', [], { input: source, encoding: 'utf8', shell: false });
  if (r.status !== 0) {
    console.error(`gofmt failed:\n${r.stderr}`);
    process.exit(2);
  }
  return r.stdout;
}

const out = gofmt(lines.join('\n'));

if (check) {
  const existing = fs.existsSync(OUT) ? fs.readFileSync(OUT, 'utf8') : '';
  const strip = (s) => s.replaceAll('\r\n', '\n').split('\n').filter((l) => !l.startsWith('// Source: tools@')).join('\n');
  if (strip(existing) !== strip(out)) {
    console.error('gate_nodes_gen.go differs from a fresh regeneration off tools pipeline-templates — node id drift; regenerate and commit.');
    process.exit(1);
  }
  console.log(`gate node consistency OK (tools@${toolsSha.slice(0, 7)})`);
} else {
  fs.writeFileSync(OUT, out, 'utf8');
  console.log(`generated ${OUT} (source tools@${toolsSha.slice(0, 7)})`);
}
