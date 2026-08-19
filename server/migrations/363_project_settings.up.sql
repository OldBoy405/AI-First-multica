-- CR-2026-004: per-project settings bag, mirroring workspace.settings.
-- First key: team_agent_queue_limit (int) — shared Team Agent queue capacity;
-- missing/invalid values fall back to the code-side default (50).
ALTER TABLE project ADD COLUMN settings JSONB NOT NULL DEFAULT '{}';
