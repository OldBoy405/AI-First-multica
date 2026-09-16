-- AIFIRST: CR-2026-049 TASK-05: trace timeline lookup by spec (SDD §2.2/FR-5).
-- Expression index locates the spec first, then orders by (occurred_at,id).
CREATE INDEX CONCURRENTLY cr_sync_event_trace_spec_idx
    ON cr_sync_event (workspace_id, (payload->>'spec_id'), occurred_at, id)
    WHERE event_kind = 'trace';
