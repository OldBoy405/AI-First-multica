package service

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/maturity"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestMaturityServiceReadPath(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)

	wsID := seedMaturityFixture(t, ctx, pool)
	planTime := time.Date(2026, 8, 20, 0, 30, 0, 0, shanghaiLoc)
	if _, err := RollupMaturityWorkspace(ctx, pool, wsID, planTime); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	// Seed an older org row after the latest one. Overall(nil) must still return
	// the latest bucket, never ASC LIMIT 1's oldest row.
	if _, err := pool.Exec(ctx, `
		INSERT INTO maturity_snapshot (workspace_id,bucket_date,scope,scope_id,metrics,scores,config_rev)
		SELECT workspace_id, bucket_date - 1, scope, scope_id, metrics, scores, config_rev
		FROM maturity_snapshot WHERE workspace_id=$1 AND scope='org' AND bucket_date='2026-08-19'`, wsID); err != nil {
		t.Fatalf("seed older snapshot: %v", err)
	}

	prices, _ := maturity.GeneratedPriceMap()
	svc := NewMaturityService(db.New(pool), maturity.GeneratedConfig(), prices)

	// overall: ready, observing -> scores empty, 8 metrics visible.
	overall, err := svc.Overall(ctx, wsID, nil)
	if err != nil {
		t.Fatalf("overall: %v", err)
	}
	if overall.DataStatus != "ready" || overall.BucketDate != "2026-08-19" {
		t.Fatalf("overall = %+v", overall)
	}
	if overall.TotalScore != nil {
		t.Fatalf("observing overall must have total_score=null, got %v", *overall.TotalScore)
	}
	if len(overall.Dimensions) != 5 || len(overall.Governance) != 6 {
		t.Fatalf("dimensions=%d governance=%d", len(overall.Dimensions), len(overall.Governance))
	}
	// The fixture CR carries a free-text owner: collab must be unavailable,
	// never a fabricated number.
	var collab *maturity.MetricValue
	for _, d := range overall.Dimensions {
		for _, m := range d.Metrics {
			if m.Key == maturity.MetricProjectCollabScale {
				v := m.Raw
				collab = &v
			}
		}
	}
	if collab == nil || collab.DataStatus != maturity.StatusUnavailable || collab.Reason == nil || *collab.Reason != reasonOwnerUnresolved {
		t.Fatalf("collab = %+v, want unavailable/%s", collab, reasonOwnerUnresolved)
	}

	// rankings: project scope, one project with valid token metrics.
	rankings, err := svc.Rankings(ctx, wsID, nil, "total", 20, nil)
	if err != nil {
		t.Fatalf("rankings: %v", err)
	}
	if rankings.Scope != "project" || len(rankings.Items) < 1 {
		t.Fatalf("rankings = %+v", rankings)
	}
	// metric=total with empty scores -> unavailable, not a fake total.
	if rankings.Items[0].Value != nil {
		t.Fatalf("observing rankings total must be null, got %v", *rankings.Items[0].Value)
	}
	if _, err := svc.Rankings(ctx, wsID, nil, "unknown_metric", 20, nil); !IsMaturityInvalidQuery(err) {
		t.Fatalf("unknown ranking metric error = %v, want invalid query", err)
	}
	badCursor := "not-base64!"
	if _, err := svc.Rankings(ctx, wsID, nil, "total", 20, &badCursor); !IsMaturityInvalidQuery(err) {
		t.Fatalf("bad ranking cursor error = %v, want invalid query", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE maturity_snapshot SET scores='{"metric_scores":"invalid"}'::jsonb WHERE workspace_id=$1 AND scope='project'`, wsID); err != nil {
		t.Fatalf("corrupt project scores: %v", err)
	}
	if _, err := svc.Rankings(ctx, wsID, nil, "total", 20, nil); err == nil {
		t.Fatal("corrupt project ranking scores must return an error")
	}
	if _, err := pool.Exec(ctx, `UPDATE maturity_snapshot SET scores='{}'::jsonb WHERE workspace_id=$1 AND scope='project'`, wsID); err != nil {
		t.Fatalf("restore project scores: %v", err)
	}

	// token-trend: project series from the snapshot.
	from := time.Date(2026, 8, 18, 0, 0, 0, 0, shanghaiLoc)
	to := time.Date(2026, 8, 20, 0, 0, 0, 0, shanghaiLoc)
	trend, err := svc.TokenTrend(ctx, wsID, maturity.TokenTrendQuery{Dimension: "project", From: from, To: to})
	if err != nil {
		t.Fatalf("token-trend: %v", err)
	}
	if trend.DataStatus != "ready" || len(trend.Series) == 0 {
		t.Fatalf("trend = %+v", trend)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_usage (task_id, provider, model, input_tokens, output_tokens, created_at)
		SELECT q.id, 'openai', 'gpt-5.6', 40, 10, '2026-08-18 00:00:00+08'
		FROM agent_task_queue q
		JOIN agent a ON a.id=q.agent_id AND a.workspace_id=$1
		LEFT JOIN task_usage tu ON tu.task_id=q.id
		WHERE tu.id IS NULL LIMIT 1`, wsID); err != nil {
		t.Fatalf("seed second model day: %v", err)
	}
	modelTrend, err := svc.TokenTrend(ctx, wsID, maturity.TokenTrendQuery{
		Dimension: "model", DimensionID: "openai:gpt-5.6", From: from, To: to, IncludeCost: true,
	})
	if err != nil {
		t.Fatalf("model trend: %v", err)
	}
	if len(modelTrend.Series) != 1 || len(modelTrend.Series[0].Points) != 2 ||
		modelTrend.Series[0].Points[0].Date != "2026-08-18" || modelTrend.Series[0].Points[1].Date != "2026-08-19" {
		t.Fatalf("model trend must be daily: %+v", modelTrend)
	}

	// config: generated seed, no price map.
	cfg, err := svc.Config(ctx, wsID)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.CalibrationStatus != "observing" || len(cfg.Metrics) != 8 || cfg.PriceConfigRev != nil {
		t.Fatalf("config = %+v", cfg)
	}
	for _, metric := range cfg.Metrics {
		if metric.KnownGameability == "" {
			t.Fatalf("metric %s missing known gameability", metric.Key)
		}
	}

	// suggestions: no org-admin project -> empty.
	sugg, err := svc.Suggestions(ctx, wsID)
	if err != nil || sugg.DataStatus != "empty" {
		t.Fatalf("suggestions = %+v, %v", sugg, err)
	}
	hist, err := svc.SuggestionHistory(ctx, wsID, 12, nil)
	if err != nil || hist.DataStatus != "empty" || len(hist.Items) != 0 {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	badReportCursor := "not-base64!"
	if _, err := svc.SuggestionHistory(ctx, wsID, 12, &badReportCursor); !IsMaturityInvalidQuery(err) {
		t.Fatalf("bad report cursor error = %v, want invalid query", err)
	}

	// Persisted projection corruption must fail closed, never masquerade as an
	// observation-period empty score payload.
	if _, err := pool.Exec(ctx, `UPDATE maturity_snapshot SET scores='{"metric_scores":"invalid"}'::jsonb WHERE workspace_id=$1 AND scope='org' AND bucket_date='2026-08-19'`, wsID); err != nil {
		t.Fatalf("corrupt scores fixture: %v", err)
	}
	if _, err := svc.Overall(ctx, wsID, nil); err == nil {
		t.Fatal("corrupt snapshot scores must return an error")
	}

	// empty workspace: overall empty, no crash.
	emptyWS := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace (id, name, slug) VALUES ($1,'empty',$2)`, uuid.UUID(emptyWS.Bytes), "empty-"+uuid.UUID(emptyWS.Bytes).String()[:8]); err != nil {
		t.Fatalf("seed empty ws: %v", err)
	}
	overallEmpty, err := svc.Overall(ctx, emptyWS, nil)
	if err != nil {
		t.Fatalf("empty overall: %v", err)
	}
	if overallEmpty.DataStatus != "empty" {
		t.Fatalf("empty overall = %+v", overallEmpty)
	}
}
