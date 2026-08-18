-- Single-statement CONCURRENTLY per repo convention.
DROP INDEX CONCURRENTLY IF EXISTS idx_agent_task_queue_pipeline_node_active;
