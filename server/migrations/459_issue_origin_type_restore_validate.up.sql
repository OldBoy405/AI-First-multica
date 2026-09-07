-- CR-2026-045 TASK-15: validate the repaired complete issue origin set
-- (now ten values incl. telegram_chat).
ALTER TABLE issue VALIDATE CONSTRAINT issue_origin_type_check;
