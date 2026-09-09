# Project chat composer layout — differences from the ordinary chat composer

CR-2026-062 aligns the Team Agent and Private Ask project send boxes with the
ordinary (non-project) chat composer while deliberately keeping a few
necessary differences. This document is the required deliverable that records
each one, whether it is a necessary difference, and why (SDD-CLOSE-04).

## a) Message column, queue bar, and composer share a nested GUTTER > COLUMN structure

The project panes' message stream, queue bar, banner zones, and the
`ChatInputCore` surface are all rendered as **two nested DOM layers**:

```text
<div className={CHAT_GUTTER}>   // px-5 @2xl:px-8 @4xl:px-12 — OUTSIDE the cap
  <div className={CHAT_COLUMN}> // mx-auto w-full max-w-4xl — the reading column
```

The gutter stays outside the `max-w-4xl` cap, so it acts as a floor that only
bites while the surface is narrower than the cap; past the cap the column
parks centered. This is the exact geometry of the ordinary chat column
(`packages/views/chat/components/chat-column.ts`), whose header comment
documents the historical single-element shape (`mx-auto max-w-4xl px-5`) that
capped the text 40px narrower than the composer card and made the two edges
visibly fail to line up.

- **Necessary difference?** No difference in geometry — the project panes now
  reuse the shared constants (`CHAT_GUTTER` / `CHAT_COLUMN`) and the same
  nesting order. It is listed here because the Team Agent pane previously
  used a single-element `mx-auto max-w-3xl px-4` column, so both the nesting
  and the cap width (max-w-3xl → max-w-4xl) change for project chat. The
  change is required by AC-1: the message column and the composer surface
  must line up exactly.
- **Single-element merge is forbidden**: putting both token groups on one
  element re-creates the historical offset (gutter padding inside the cap).

## b) Flow footer vs. the ordinary composer's absolute action row

The ordinary `ChatInput` pins its action row with absolute positioning
(`bottom-1.5 left-1.5` / `bottom-1 right-1.5`) over a `pb-9` reservation.
`ChatInputCore` now renders the footer as a normal **flow row** between the
editor area and the card bottom:

```text
editor area (flex-1 min-h-8 overflow-y-auto px-3 py-2)
footer row (flex items-center justify-between gap-2 px-1.5 pb-1.5)
  ├─ left group (flex min-w-0 flex-1 flex-wrap items-center gap-1)
  │    ChatAddMenu + leftAdornment (model / thinking chips)
  └─ right group (flex shrink-0 items-center gap-1) — SubmitButton
```

- **Necessary difference?** Yes. The ordinary composer only carries an
  add-menu; the project composers additionally carry model/thinking chips.
  With an absolute row, the chips would overlap the editor on a 360px pane.
  The flow row wraps inside its own line (pure CSS `flex-wrap` — no JS
  measurement, no ResizeObserver, no state), so the editor never gets
  squeezed and the send/stop button stays pinned bottom-right. In the
  one-line state the two layouts are visually equivalent (same 6px
  bottom-left inset via `px-1.5 pb-1.5`). The editor keeps an explicit
  `min-h-8` floor so it stays operable while the footer wraps.
- **Necessary difference?** The editor floor (`min-h-8` instead of the
  ordinary `min-h-0`) is required for FR-4: a wrapped footer must not
  squeeze the input area.

## c) Surface height cap `max-h-40` (ordinary composer: `max-h-96`)

`ChatInputCore` keeps its existing `max-h-40` cap and does not adopt the
ordinary composer's `max-h-96` / `max-h-[50%]` system.

- **Necessary difference?** Yes. The project pane is a short viewport that
  also hosts the message stream and the queue bar. A 24rem composer would
  crowd the stream out of the panel. AC-2 (long text scrolls inside, the
  send button stays in view) is already satisfied by `max-h-40` +
  `overflow-y-auto` on the editor area.

## d) Model / Thinking labels are no longer always visible; chips + sr-only categories

The independent `*-model-row` rows are removed. Model and Thinking controls
now live in the composer footer's left group (via `leftAdornment`), rendered
as the existing `chip` variant of `ModelPicker` / `ThinkingPicker`. The
visible text labels (`model_label` / `thinking_label`) are no longer rendered
permanently; each control is wrapped with an `sr-only` span carrying the same
existing locale key, so assistive technology still reads "Model <value>" /
"Thinking <value>".

- **Necessary difference?** Yes — the second visual system (a standalone
  config row above the surface) is precisely what this CR removes (FR-1/
  AC-4). The value and the category semantics survive: editable chips carry
  their own `aria-label` + tooltip; read-only chips are plain spans whose
  category name is restored by the sr-only label. No new locale keys.
- **testid contract**: `private-ask-model-row` is removed with its row and
  replaced by the new `private-ask-model-picker` anchor;
  `project-chat-model-row` is removed with its row and has **no replacement
  anchor** — assertions use the four inner testids
  (`project-chat-model-readonly` / `project-chat-model-picker` /
  `project-chat-model-runtime-guide` / `project-chat-thinking-picker`)
  scoped to the `project-chat-composer` subtree. `private-ask-thinking-picker`
  and every other existing testid keep their values.

## e) Team Agent stop targets the most recently sent task only

The composer's bottom-right stop now acts on the **last task this composer
sent** (`sentTaskId` / `sentIssueId` recorded from the send response), via the
existing `useCancelProjectQueueTask` mutation (TSUG-007 semantics). Activity
is judged from two existing, already-cached sources — zero new API calls:

| stage | queue items (server-filtered queued + dispatched) | container issue task timeline (`GET /api/issues/:id/task-runs`) | tracked active |
|---|---|---|---|
| queued | contains sentTaskId | `tasks.find(t => t.id === sentTaskId)` → `queued` | true |
| dispatched | contains sentTaskId | → `dispatched` | true |
| running | absent (filtered out) | → `running` | true (timeline alone) |
| waiting_local_directory | absent | → `waiting_local_directory` | true (timeline alone) |
| completed / failed / cancelled | absent | → terminal | **false** (terminal wins over stale items) |
| no hit / empty list | may or may not contain | `taskEntry = undefined` | **items-only** — never another task's state |

- `tasks` is the `AgentTask[]` array returned by `api.listTasksByIssue`
  (parse failure falls back to `[]`); the entry is picked strictly by
  `tasks.find(task => task.id === sentTaskId)` — never first, never by time.
  A miss (empty list, query disabled/loading, or the task not yet in the
  timeline) degrades to items-only; a container issue's OTHER tasks can never
  leak their running/terminal state into this composer.
- **Deterministic transfer**: a successful send with valid `task_id` +
  `issue_id` pins the new target; a successful send with either id empty
  atomically clears both (no dead stop button, no cancel of a stale task); a
  failed send keeps the previous target.
- **Visible affordance** (same formula as the ordinary queue-capable
  composer): while active, an empty draft (or uploads in flight) renders
  **stop**; live content renders **send (queue send)** because Team Agent
  passes `allowSubmitWhileRunning`. A failed send keeps the draft, so the
  affordance is the send/retry button; clearing the draft brings stop back
  and it still cancels the retained target. Terminal status always returns
  the button to send.
- **Necessary difference?** Yes vs. the previous state (a stop button with no
  handler + no running state after enqueue), and yes vs. the ordinary chat
  composer (whose single task is the session's own run): project chat is a
  shared queue, so the composer only owns its most recent send — the other
  queued tasks keep being managed per-item through the queue bar.
- Private Ask deliberately keeps its existing stop path unchanged
  (`pendingTaskId → api.cancelTaskById`) and does **not** pass
  `allowSubmitWhileRunning`: it stays stop-only while running, byte-for-byte
  the pre-CR behavior.
