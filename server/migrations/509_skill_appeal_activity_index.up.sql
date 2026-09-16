-- AIFIRST: CR-2026-048 TASK-01: appeal lookup index on the hot activity_log table.
-- Appeal rows carry issue_id = NULL, so every existing activity_log index
-- (all issue_id-led: 068 keyset, 089 squad) misses them and the appeal lookup
-- degenerates into a full scan. Partial expression index, same shape as 089.
CREATE INDEX CONCURRENTLY skill_appeal_activity_idx
    ON activity_log ((details->>'appeal_id'))
    WHERE action IN ('skill_appeal_submitted','skill_appeal_approved','skill_appeal_rejected');
