-- AIFIRST: CR-2026-049 TASK-05: cr_sync_event workspace scoping (SDD §2.2/§2.4).
-- Deterministic preflight: a cr_id must map to exactly one cr.workspace_id
-- (count<>1 blocks both orphans (0) and multi-tenant collisions (>1)); the
-- backfill then uses a scalar subquery and asserts zero NULL rows before
-- SET NOT NULL. No arbitrary pick via UPDATE ... FROM.
ALTER TABLE cr_sync_event ADD COLUMN workspace_id UUID;

DO $$
BEGIN
  IF EXISTS (
    SELECT e.cr_id
    FROM cr_sync_event e
    LEFT JOIN cr c ON c.cr_id = e.cr_id
    GROUP BY e.cr_id
    HAVING count(DISTINCT c.workspace_id) <> 1
  ) THEN RAISE EXCEPTION 'cr_sync_event workspace backfill is ambiguous or orphaned';
  END IF;
END $$;

UPDATE cr_sync_event e
SET workspace_id = (SELECT c.workspace_id FROM cr c WHERE c.cr_id = e.cr_id);

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM cr_sync_event WHERE workspace_id IS NULL)
  THEN RAISE EXCEPTION 'cr_sync_event workspace backfill left null rows';
  END IF;
END $$;

ALTER TABLE cr_sync_event ALTER COLUMN workspace_id SET NOT NULL;
