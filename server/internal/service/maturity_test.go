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

	// config: generated seed, no price map.
	cfg, err := svc.Config(ctx, wsID)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.CalibrationStatus != "observing" || len(cfg.Metrics) != 8 || cfg.PriceConfigRev != nil {
		t.Fatalf("config = %+v", cfg)
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
