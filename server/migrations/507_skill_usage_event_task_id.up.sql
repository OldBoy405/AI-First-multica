-- AIFIRST: CR-2026-048 TASK-01: completed-task filter index for usage aggregation.
CREATE INDEX CONCURRENTLY skill_usage_event_task_id_idx ON skill_usage_event(task_id);
