package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestResolveTaskWorkspaceIDPipelineCarrier(t *testing.T) {
	svc := &TaskService{}
	task := db.AgentTaskQueue{Context: []byte(`{"type":"pipeline_node","schema":"ai-first.pipeline-task/v1","workspace_id":"00000000-0000-0000-0000-000000000045"}`)}
	if got := svc.ResolveTaskWorkspaceID(context.Background(), task); got != "00000000-0000-0000-0000-000000000045" {
		t.Fatalf("unexpected workspace %q", got)
	}
	task.Context = []byte(`{"type":"pipeline_node","schema":"wrong","workspace_id":"unsafe"}`)
	if got := svc.ResolveTaskWorkspaceID(context.Background(), task); got != "" {
		t.Fatalf("invalid carrier must fail closed, got %q", got)
	}
}

func TestNotifyTaskAvailableAllowsDisabledEmptyClaimCache(t *testing.T) {
	svc := &TaskService{}
	svc.notifyTaskAvailable(db.AgentTaskQueue{RuntimeID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}})
}
