package handler

import "testing"

func TestHydratePipelineContext(t *testing.T) {
	var response AgentTaskResponse
	err := hydratePipelineContext([]byte(`{
		"type":"pipeline_node","schema":"ai-first.pipeline-task/v1",
		"pipeline_id":"architecture-design","cr_id":"CR-2026-045",
		"run_id":"00000000-0000-0000-0000-000000000001",
		"node_id":"00000000-0000-0000-0016-000000000001",
		"attempt":2,"prompt":"fixed"
	}`), &response)
	if err != nil {
		t.Fatal(err)
	}
	if response.PipelinePrompt != "fixed" || response.PipelineCrID != "CR-2026-045" || response.PipelineAttempt != 2 {
		t.Fatalf("pipeline carrier mismatch: %+v", response)
	}
}

func TestHydratePipelineContextRejectsIncompleteDeclaredContext(t *testing.T) {
	var response AgentTaskResponse
	if err := hydratePipelineContext([]byte(`{"type":"pipeline_node","schema":"ai-first.pipeline-task/v1"}`), &response); err == nil {
		t.Fatal("declared pipeline context must fail closed when incomplete")
	}
	if err := hydratePipelineContext([]byte(`{"type":"pipeline_node","schema":"ai-first.pipeline-task/v1","pipeline_id":"architecture-design","cr_id":"../../escape","run_id":"bad","node_id":"bad","attempt":4,"prompt":"x"}`), &response); err == nil {
		t.Fatal("declared pipeline identifiers must fail closed")
	}
	if err := hydratePipelineContext([]byte(`{"type":"ordinary"}`), &response); err != nil {
		t.Fatalf("ordinary context must stay backward compatible: %v", err)
	}
}
