-- AIFIRST: CR-2026-059 TASK-01 (SDD §2.5, FR-22 author columns): chat_message
-- gains nullable author columns so a shared Discussion session can attribute
-- each message to its sender. No FK (application-level validation, invariant
-- 6). Existing Private Ask / 1:1 rows stay NULL (creator-only semantics; the
-- frontend keeps today's rendering, no backfill).
ALTER TABLE chat_message
    ADD COLUMN author_type TEXT,
    ADD COLUMN author_id UUID;
