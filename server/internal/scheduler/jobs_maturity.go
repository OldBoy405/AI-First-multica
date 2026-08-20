package scheduler

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// JobNameMaturitySnapshot is the stable audit key of the maturity snapshot job.
const JobNameMaturitySnapshot = "maturity_snapshot"

var maturityShanghaiLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("Asia/Shanghai", 8*3600)
	}
	return loc
}()

// MaturitySnapshotJob builds the daily 00:30 Asia/Shanghai maturity snapshot
// job. The Cadence-based planner cannot express a local-midnight+30m grid, so
// planning is fully hook-driven: CatchUpMode/CatchUpWindow are declared as
// intent only and the scheduler ignores them while PlansForScope is set
// (SDD §3.7). Timing/lease/retry fields mirror AutopilotScheduleDispatchJob.
func MaturitySnapshotJob(pool *pgxpool.Pool) JobSpec {
	queries := db.New(pool)
	return JobSpec{
		Name:          JobNameMaturitySnapshot,
		Cadence:       0, // hook-driven
		ScheduleDelay: 0,
		// Declared intent only — with PlansForScope set the scheduler never
		// reads these two fields; the real compensation logic lives in the hook.
		CatchUpMode:   CatchUpEveryPlan,
		CatchUpWindow: 7 * 24 * time.Hour,
		// One plan per local day, up to a week of catch-up + retries.
		MaxPlansPerTick: 7,
		RunTimeout:      2 * time.Minute,
		StaleTimeout:    5 * time.Minute,
		HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true,
		MaxAttempts:       3,
		RetryBackoff: []time.Duration{
			1 * time.Minute,
			5 * time.Minute,
			15 * time.Minute,
		},
		Scopes:        StaticScopes(ScopeGlobal),
		PlansForScope: maturityPlansForScope(queries),
		Handler:       maturitySnapshotHandler(pool),
	}
}

// maturityPlansForScope enumerates due plan_times (canonical UTC):
//  1. retry-eligible FAILED plans from sys_cron_executions within the 7-day
//     window (oldest first, cap 7) — a failed plan must never be stranded
//     behind a newer success;
//  2. fresh cron occurrences of '30 0 * * *' Asia/Shanghai after the latest
//     known plan (or now-24h on first deployment, which yields only the most
//     recent due plan instead of fabricating pre-launch observation days);
//  3. union, oldest-first, truncated to MaxPlansPerTick=7.
func maturityPlansForScope(queries *db.Queries) func(context.Context, Scope, time.Time, LatestPlanInfo) ([]time.Time, error) {
	return func(ctx context.Context, _ Scope, now time.Time, latest LatestPlanInfo) ([]time.Time, error) {
		now = now.UTC()
		windowStart := now.Add(-7 * 24 * time.Hour)

		retryRows, err := queries.MaturityRetryablePlans(ctx, db.MaturityRetryablePlansParams{
			NextRetryAt: pgtype.Timestamptz{Time: now, Valid: true},
			PlanTime:    pgtype.Timestamptz{Time: windowStart, Valid: true},
		})
		if err != nil {
			return nil, fmt.Errorf("maturity plans: retry scan: %w", err)
		}

		planSet := map[time.Time]bool{}
		for _, r := range retryRows {
			if r.Valid {
				planSet[r.Time.UTC()] = true
			}
		}

		after := now.Add(-24 * time.Hour)
		if latest.Found {
			after = latest.PlanTime.UTC()
		}
		if after.Before(windowStart) {
			after = windowStart
		}
		occs, err := service.NextOccurrencesUTC("30 0 * * *", "Asia/Shanghai", after, now)
		if err != nil {
			return nil, fmt.Errorf("maturity plans: cron eval: %w", err)
		}
		for _, o := range occs {
			planSet[o.UTC()] = true
		}

		plans := make([]time.Time, 0, len(planSet))
		for p := range planSet {
			plans = append(plans, p)
		}
		sort.Slice(plans, func(i, j int) bool { return plans[i].Before(plans[j]) })
		if len(plans) > 7 {
			plans = plans[:7]
		}
		return plans, nil
	}
}

// maturitySnapshotHandler rolls up one plan. Each plan covers exactly the
// Shanghai-local day BEFORE its plan_time (SDD §4.5); the handler never loops
// additional buckets.
func maturitySnapshotHandler(pool *pgxpool.Pool) Handler {
	return func(ctx context.Context, in HandlerInput) (HandlerResult, error) {
		rows, err := service.RollupMaturitySnapshot(ctx, pool, in.PlanTime)
		if err != nil {
			return HandlerResult{}, err
		}
		bucket := in.PlanTime.In(maturityShanghaiLoc).AddDate(0, 0, -1).Format("2006-01-02")
		return HandlerResult{
			RowsAffected: rows,
			Result:       map[string]any{"bucket_date": bucket, "rows": rows},
		}, nil
	}
}
