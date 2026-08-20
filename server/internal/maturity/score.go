package maturity

import (
	"fmt"
	"time"
)

// MetricScore clamps the linear score of one raw metric value
// (SDD §4.4): clamp(100*(x-floor)/(target-floor), 0, 100). NaN reads as 0.
func MetricScore(x float64, c MetricConfig) float64 {
	if x != x { // NaN
		return 0
	}
	s := 100 * (x - c.Floor) / (c.Target - c.Floor)
	switch {
	case s < 0:
		return 0
	case s > 100:
		return 100
	default:
		return s
	}
}

// scoringReady reports whether a metric value may enter the score. Any
// non-ready or null value disqualifies the whole scope/date: callers store an
// empty scores object instead of partially renormalizing weights (SDD §4.4).
func scoringReady(mv MetricValue) bool {
	return mv.DataStatus == StatusReady && mv.Value != nil
}

// DimensionScores returns one score per dimension. Any non-ready scoring
// metric is an error — no partial weight renormalization.
func DimensionScores(m map[MetricKey]MetricValue, cfg ConfigV1) (map[DimensionKey]float64, error) {
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	out := make(map[DimensionKey]float64, len(cfg.Dimensions))
	for d, keys := range cfg.Dimensions {
		weighted, weightSum := 0.0, 0.0
		for _, k := range keys {
			mv, ok := m[k]
			if !ok {
				return nil, fmt.Errorf("missing metric value for %q", k)
			}
			if !scoringReady(mv) {
				return nil, fmt.Errorf("metric %q is not ready (data_status=%s): refusing partial renormalization", k, mv.DataStatus)
			}
			mc := cfg.Metrics[k]
			weighted += MetricScore(*mv.Value, mc) * mc.Weight
			weightSum += mc.Weight
		}
		out[d] = weighted / weightSum
	}
	return out, nil
}

// TotalScore is the global weighted sum (weights sum to 1). Any non-ready
// scoring metric is an error.
func TotalScore(m map[MetricKey]MetricValue, cfg ConfigV1) (float64, error) {
	if err := ValidateConfig(cfg); err != nil {
		return 0, err
	}
	total := 0.0
	for _, k := range AllMetricKeys {
		mv, ok := m[k]
		if !ok {
			return 0, fmt.Errorf("missing metric value for %q", k)
		}
		if !scoringReady(mv) {
			return 0, fmt.Errorf("metric %q is not ready (data_status=%s): refusing partial renormalization", k, mv.DataStatus)
		}
		total += MetricScore(*mv.Value, cfg.Metrics[k]) * cfg.Metrics[k].Weight
	}
	return total, nil
}

// BuildScores produces the full frozen score payload. Any missing or
// non-ready metric is an error; callers store scores={} in that case.
func BuildScores(m map[MetricKey]MetricValue, cfg ConfigV1) (SnapshotScoresV1, error) {
	dims, err := DimensionScores(m, cfg)
	if err != nil {
		return SnapshotScoresV1{}, err
	}
	total, err := TotalScore(m, cfg)
	if err != nil {
		return SnapshotScoresV1{}, err
	}
	metricScores := make(map[MetricKey]float64, len(AllMetricKeys))
	for _, k := range AllMetricKeys {
		metricScores[k] = MetricScore(*m[k].Value, cfg.Metrics[k])
	}
	return SnapshotScoresV1{
		Schema:          "ai-first.maturity-scores/v1",
		MetricScores:    metricScores,
		DimensionScores: dims,
		TotalScore:      total,
	}, nil
}

// ObservationActive reports whether scoring is suppressed: fewer than 28 days
// of history or the config has not been calibrated by CR-D human review.
func ObservationActive(firstBucket time.Time, now time.Time, cfg ConfigV1) bool {
	return now.Sub(firstBucket) < 28*24*time.Hour || cfg.CalibrationStatus != "calibrated"
}
