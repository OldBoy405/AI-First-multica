-- AIFIRST: completed maturity report history index (CR-2026-047 TASK-02,
-- SDD §2.1). Serves GET /api/maturity/suggestions history keyset pagination.
-- Migration 369's partial index covers only active tasks; completed report
-- history must not be misrouted onto it.
CREATE INDEX CONCURRENTLY idx_atq_maturity_report_history
    ON agent_task_queue (project_id, completed_at DESC, id DESC)
    WHERE status = 'completed'
      AND project_id IS NOT NULL
      AND result->>'schema' = 'ai-first.maturity-report/v1';
