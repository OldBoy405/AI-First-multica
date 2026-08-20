---
name: multica-maturity-weekly-report
description: "Use when the Org Admin weekly Autopilot fires to produce the workspace AI maturity report. Fetch the maturity snapshot API data, write the five-section report markdown to docs/org-admin/maturity-review-{YYYY-Www}.md, and return the structured report envelope for the task result."
user-invocable: false
allowed-tools: Write, Bash(multica *), Bash(mkdir *), Bash(mv *), Bash(sha256sum *)
---

# Weekly AI maturity report

Produce the weekly AI maturity review for the workspace. The data you need
already exists in frozen daily snapshots — never recompute scores from raw
events, and never write to `maturity-config.yaml`.

## Data

Call the workspace maturity API with the X-Workspace-ID of the Org Admin
workspace:

- `GET /api/maturity/overall` — latest bucket: headline, dimensions,
  governance.
- `GET /api/maturity/config` — the active metric config (weights/floors/
  targets) and `baseline_suggestions`. The server exposes suggestions only
  after the first 28 consecutive org buckets and at least 21 ready samples
  for that metric.
- In week 4, include every returned P10/P75 baseline suggestion. Metrics with
  no returned suggestion stay "unavailable" — say so, never invent a number.

## Output

Write exactly five sections to
`docs/org-admin/maturity-review-{YYYY-Www}.md` (atomic write: temp file +
rename):

1. **Individual efficiency** — token intensity, AI penetration.
2. **Team delivery** — CR throughput per capita, project collaboration scale.
3. **Knowledge compounding** — prototype direct rate, process completion rate.
4. **Risk & yield** — governance guardrails: gate first-pass rate, evidence
   drift count, traceability (unavailable until the CR-C trace channel),
   approval latency P50/P90, forbidden attempt count.
5. **Cost** — headline cost and its status (authoritative/mixed/estimated/
   unavailable). Never double-price provider-reported ticks.

Use the exact H2 headings `## Individual efficiency`, `## Team delivery`,
`## Knowledge compounding`, `## Risk & yield`, and `## Cost`. Every section
must cite the metric keys it uses. End with the anti-Goodhart note: tokens are
behavioural data, not individual performance metrics.

Use only the declared tools for this atomic temp-file + rename sequence:

1. `mkdir -p docs/org-admin`.
2. Use `Write` to create
   `docs/org-admin/.maturity-review-{YYYY-Www}.md.tmp` with the exact Markdown.
3. Run `sha256sum` on the temp file and keep the lowercase digest.
4. Run `mv -f` from the temp path to the final path. This same-directory rename
   is the atomic publish boundary on the project local directory.

Never write the final path directly, and never run `git add` or `git commit`
for the report.

Return **only JSON** as the final task output. The server validates it and
stores this direct envelope in `agent_task_queue.result` before notifying the
Owner inbox:

```json
{
  "schema": "ai-first.maturity-report/v1",
  "report_key": "<workspace-id>:<YYYY-Www>",
  "week": "<YYYY-Www>",
  "generated_at": "<RFC3339>",
  "relative_path": "docs/org-admin/maturity-review-<YYYY-Www>.md",
  "markdown": "<exact bytes written to the file>",
  "content_sha256": "<lowercase SHA-256 of markdown UTF-8 bytes>",
  "source_task_id": "<Task ID from the Autopilot prompt>",
  "chat_session_id": "<Chat session ID from the Autopilot prompt>",
  "config_revs": ["<every config revision cited>"]
}
```

Do not wrap the envelope in prose or Markdown fences. The server rejects a
wrong task/chat binding, path, missing five-section heading, or SHA mismatch.
