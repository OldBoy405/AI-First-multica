-- AIFIRST: CR-2026-049 TASK-05: drop the pre-workspace approve idempotency
-- index; approval_record_approve_ws_uniq (396) is already in place.
DROP INDEX CONCURRENTLY approval_record_approve_uniq;
