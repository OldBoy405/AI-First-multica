package maturity

import (
	"math"
	"testing"
	"time"
)

func ready(v float64) MetricValue {
	return MetricValue{Value: ptr(v), DataStatus: StatusReady}
}

func ptr(v float64) *float64 { return &v }

func calibratedConfig() ConfigV1 {
	c := validConfig()
	c.CalibrationStatus = "calibrated"
	return c
}

func allReady(overrides map[MetricKey]MetricValue) map[MetricKey]MetricValue {
	m := make(map[MetricKey]MetricValue, len(AllMetricKeys))
	for _, k := range AllMetricKeys {
		m[k] = ready(1)
	}
	for k, v := range overrides {
		m[k] = v
	}
	return m
}

func TestMetricScore(t *testing.T) {
	cases := []struct {
		x, floor, target, want float64
	}{
		{5, 0, 10, 50},
		{-1, 0, 10, 0},
		{11, 0, 10, 100},
		{0, 0, 10, 0},
		{10, 0, 10, 100},
		{math.NaN(), 0, 10, 0},
	}
	for _, tc := range cases {
		if got := MetricScore(tc.x, MetricConfig{Weight: 1, Floor: tc.floor, Target: tc.target}); got != tc.want {
			t.Errorf("MetricScore(%v, floor=%v, target=%v) = %v, want %v", tc.x, tc.floor, tc.target, got, tc.want)
		}
	}
}

func TestDimensionScores(t *testing.T) {
	cfg := calibratedConfig()
	m := allReady(nil)
	// DimAIF (canonical two-metric dim): weights 0.2/0.05 (replacing two
	// 0.125s keeps the global sum at 1), scores 20 and 80 ->
	// (20*0.2+80*0.05)/0.25 = 32.
	cfg.Metrics[MetricTokenIntensity] = MetricConfig{Weight: 0.2, Floor: 0, Target: 1}
	cfg.Metrics[MetricAIPenetration] = MetricConfig{Weight: 0.05, Floor: 0, Target: 1}
	m[MetricTokenIntensity] = ready(0.2)
	m[MetricAIPenetration] = ready(0.8)
	got, err := DimensionScores(m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got[DimAIF] != 32 {
		t.Fatalf("weighted dim score = %v, want 32", got[DimAIF])
	}

	// A single unavailable metric anywhere must error out (no renormalization).
	bad := allReady(nil)
	bad[MetricTeamAgentDepth] = MetricValue{DataStatus: StatusUnavailable}
	if _, err := DimensionScores(bad, cfg); err == nil {
		t.Fatal("expected error for unavailable metric")
	}
}

func TestTotalScore(t *testing.T) {
	cfg := calibratedConfig()
	m := allReady(nil)
	got, err := TotalScore(m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != 100 {
		t.Fatalf("all-at-target total = %v, want 100", got)
	}
	bad := allReady(nil)
	bad[MetricCRThroughputPerCapita] = MetricValue{DataStatus: StatusEmpty}
	if _, err := TotalScore(bad, cfg); err == nil {
		t.Fatal("expected error for empty metric")
	}
	delete(m, MetricProcessCompletionRate)
	if _, err := TotalScore(m, cfg); err == nil {
		t.Fatal("expected error for missing metric key")
	}
}

func TestBuildScores(t *testing.T) {
	cfg := calibratedConfig()
	s, err := BuildScores(allReady(nil), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Schema != "ai-first.maturity-scores/v1" {
		t.Fatalf("schema = %q", s.Schema)
	}
	if len(s.MetricScores) != 8 || len(s.DimensionScores) != 5 {
		t.Fatalf("key counts wrong: %d metrics, %d dims", len(s.MetricScores), len(s.DimensionScores))
	}
	for _, v := range s.MetricScores {
		if v < 0 || v > 100 {
			t.Fatalf("score %v out of range", v)
		}
	}
	if _, err := BuildScores(allReady(map[MetricKey]MetricValue{
		MetricProjectCollabScale: {DataStatus: StatusUnavailable, Reason: ptrStr("cr_owner_identity_unresolved")},
	}), cfg); err == nil {
		t.Fatal("expected error when a metric is unavailable")
	}
}

func ptrStr(s string) *string { return &s }

func TestObservationActive(t *testing.T) {
	now := time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC)
	obs := validConfig() // observing
	cal := calibratedConfig()
	cases := []struct {
		name       string
		firstBucket time.Time
		cfg        ConfigV1
		want       bool
	}{
		{"day 27 observing", now.Add(-27 * 24 * time.Hour), obs, true},
		{"day 28 observing", now.Add(-28 * 24 * time.Hour), obs, true},
		{"day 28 calibrated", now.Add(-28 * 24 * time.Hour), cal, false},
		{"day 27 calibrated", now.Add(-27 * 24 * time.Hour), cal, true},
	}
	for _, tc := range cases {
		if got := ObservationActive(tc.firstBucket, now, tc.cfg); got != tc.want {
			t.Errorf("%s: ObservationActive = %v, want %v", tc.name, got, tc.want)
		}
	}
}
