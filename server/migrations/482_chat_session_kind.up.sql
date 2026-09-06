-- AIFIRST: CR-2026-059 TASK-01 (SDD §2.2, FR-2): chat_session.kind splits
-- the Discussion shared session ('project_shared') from Private Ask / 1:1
-- rows. A constant default keeps every existing row 'private' without a
-- table rewrite; existing CreateChatSession callers keep the column default.
ALTER TABLE chat_session
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'private'
    CHECK (kind IN ('private', 'project_shared'));
