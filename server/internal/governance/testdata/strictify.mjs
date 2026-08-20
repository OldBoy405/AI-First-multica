// strictify.mjs — 一次性工具：把 KB 基线 traceability.yml 中未加引号、值内带
// ": " 的 plain scalar 加引号，生成严格 YAML fixture（跨语言 golden 的 yaml.v3 侧输入）。
// 只改语法不改语义；加引号前后两个解析器产出相同语义树。
import fs from 'node:fs';

const src = process.argv[2];
const dst = process.argv[3];
const t = fs.readFileSync(src, 'utf8').replace(/\r\n/g, '\n');
const lines = t.split('\n');
let changed = 0;
const out = lines.map((l) => {
  if (/^\s*#/.test(l) || l.trim() === '') return l;
  const m = /^(\s*)([A-Za-z0-9_-]+):\s+(.+)$/.exec(l);
  if (!m) return l;
  const v = m[3];
  if (/^["']/.test(v) || /^[\[{]/.test(v) || /^[|>]/.test(v)) return l;
  if (/: /.test(v)) {
    const q = '"' + v.replace(/\\/g, '\\\\').replace(/"/g, '\\"') + '"';
    changed++;
    return m[1] + m[2] + ': ' + q;
  }
  return l;
});
console.log('changed lines:', changed);
fs.writeFileSync(dst, out.join('\n') + '\n');
