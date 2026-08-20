-- AIFIRST: CR-2026-048 TASK-01 (Skill Market): append-only usage telemetry.
-- used_at records DISPATCH-TIME MATERIALIZATION, not completion-time use
-- (PRD FR-7): a claim writes one row per skill ref, retries write more rows.
-- All usage/reuse/ranking aggregations MUST join agent_task_queue and filter
-- status='completed', counting DISTINCT task_id per skill_ref.
-- No foreign keys on purpose: telemetry rows pointing at deleted skills are
-- audit history and must be kept (repo hard rule: no new FK/cascades).
CREATE TABLE skill_usage_event (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    skill_ref TEXT NOT NULL,
    task_id UUID,
    project_id UUID,
    used_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
