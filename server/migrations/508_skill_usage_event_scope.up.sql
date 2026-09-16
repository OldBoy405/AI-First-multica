-- AIFIRST: CR-2026-048 TASK-01: workspace-scoped ranking index.
-- workspace_id leads so ranking never mixes tenants (ARCHITECTURE.md hard invariant 1).
CREATE INDEX CONCURRENTLY skill_usage_event_scope_idx ON skill_usage_event(workspace_id, skill_ref, used_at);
