---
name: multica-maturity-weekly-report
description: "Use when the Org Admin weekly Autopilot fires to produce the workspace AI maturity report. Fetch the maturity snapshot API data, write the five-section report markdown to docs/org-admin/maturity-review-{YYYY-Www}.md, and return the structured report envelope for the task result."
user-invocable: false
allowed-tools: Bash(multica *)
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
  targets).
- In week 4 only: the baseline suggestions prepared from the first 28
  org snapshots (P10/P75 per metric). Metrics without 21+ ready samples stay
  "unavailable" — say so, never invent a number.

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

Every section must cite the metric keys it uses. End with the anti-Goodhart
note: tokens are behavioural data, not individual performance metrics.

The report body must also be returned as the task result envelope with
schema `ai-first.maturity-report/v1` (report_key = workspaceId:YYYY-Www).
