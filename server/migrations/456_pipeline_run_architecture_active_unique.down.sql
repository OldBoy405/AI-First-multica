-- Single-statement CONCURRENTLY per repo convention.
DROP INDEX CONCURRENTLY IF EXISTS idx_pipeline_run_architecture_active_cr;
