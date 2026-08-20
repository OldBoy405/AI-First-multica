package service

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/maturity"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestMaturityConfigExposesTwentyEightDayBaseline(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)

	workspaceID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace (id,name,slug) VALUES ($1,'baseline',$2)`,
		workspaceID, "baseline-"+uuid.UUID(workspaceID.Bytes).String()[:8]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM maturity_snapshot WHERE workspace_id=$1`, workspaceID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM workspace WHERE id=$1`, workspaceID)
	})
	for day := 0; day < 28; day++ {
		metrics := maturity.SnapshotMetricsV1{
			Schema:       maturityMetricsSchema,
			MetricValues: map[maturity.MetricKey]maturity.MetricValue{},
			Governance:   map[maturity.GovernanceMetricKey]maturity.MetricValue{},
		}
		for index, key := range maturity.AllMetricKeys {
			value := float64(day*10 + index)
			metrics.MetricValues[key] = maturity.MetricValue{Value: &value, Unit: unitFor(key), DataStatus: maturity.StatusReady}
		}
		for _, key := range maturity.AllGovernanceKeys {
			metrics.Governance[key] = metricEmpty("ratio")
		}
		payload, err := json.Marshal(metrics)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO maturity_snapshot
			(workspace_id,bucket_date,scope,scope_id,metrics,scores,config_rev)
			VALUES ($1,$2,'org','·',$3,'{}',$4)`,
			workspaceID, time.Date(2026, 7, 1+day, 0, 0, 0, 0, time.UTC), payload, strings.Repeat("a", 40)); err != nil {
			t.Fatal(err)
		}
	}
	service := NewMaturityService(db.New(pool), maturity.GeneratedConfig(), maturity.PriceMap{})
	config, err := service.Config(ctx, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.BaselineSuggestions) != len(maturity.AllMetricKeys) {
		t.Fatalf("baseline suggestions = %d, want %d", len(config.BaselineSuggestions), len(maturity.AllMetricKeys))
	}
	for _, suggestion := range config.BaselineSuggestions {
		if suggestion.SampleCount != 28 || suggestion.FloorP10 >= suggestion.TargetP75 {
			t.Fatalf("invalid baseline suggestion: %+v", suggestion)
		}
	}
}
