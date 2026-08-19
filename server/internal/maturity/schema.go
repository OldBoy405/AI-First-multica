// Package maturity holds the AI maturity dashboard (CR-A, CR-2026-047) domain
// types, the generated config copy and pure scoring functions. It never
// touches the database or HTTP — all IO lives in internal/service and
// internal/handler.
package maturity

import (
	"fmt"
	"math"
)

// MetricKey identifies one of the eight fixed maturity sub-metrics (SDD §2.4).
type MetricKey string

const (
	MetricTokenIntensity        MetricKey = "token_intensity"
	MetricAIPenetration         MetricKey = "ai_penetration"
	MetricCRThroughputPerCapita MetricKey = "cr_throughput_per_capita"
	MetricProjectCollabScale    MetricKey = "project_collab_scale"
	MetricProjectActiveRate     MetricKey = "project_active_rate"
	MetricPrototypeDirectRate   MetricKey = "prototype_direct_rate"
	MetricTeamAgentDepth        MetricKey = "team_agent_depth"
	MetricProcessCompletionRate MetricKey = "process_completion_rate"
)

// AllMetricKeys lists the eight fixed keys in canonical order.
var AllMetricKeys = []MetricKey{
	MetricTokenIntensity,
	MetricAIPenetration,
	MetricCRThroughputPerCapita,
	MetricProjectCollabScale,
	MetricProjectActiveRate,
	MetricPrototypeDirectRate,
	MetricTeamAgentDepth,
	MetricProcessCompletionRate,
}

// DimensionKey identifies one of the five fixed dimensions.
type DimensionKey string

const (
	DimAIF DimensionKey = "AIF"
	DimSII DimensionKey = "SII"
	DimOFI DimensionKey = "OFI"
	DimEPC DimensionKey = "EPC"
	DimACM DimensionKey = "ACM"
)

// GovernanceMetricKey identifies one of the six governance guardrails (SDD §4.3).
// Governance metrics never enter the total score.
type GovernanceMetricKey string

const (
	GovGateFirstPassRate       GovernanceMetricKey = "gate_first_pass_rate"
	GovEvidenceDriftCount      GovernanceMetricKey = "evidence_drift_count"
	GovTraceabilityCompleteRate GovernanceMetricKey = "traceability_complete_rate"
	GovApprovalLatencyP50      GovernanceMetricKey = "approval_latency_p50_ms"
	GovApprovalLatencyP90      GovernanceMetricKey = "approval_latency_p90_ms"
	GovForbiddenAttemptCount   GovernanceMetricKey = "forbidden_attempt_count"
)

// AllGovernanceKeys lists the six fixed governance keys in canonical order.
var AllGovernanceKeys = []GovernanceMetricKey{
	GovGateFirstPassRate,
	GovEvidenceDriftCount,
	GovTraceabilityCompleteRate,
	GovApprovalLatencyP50,
	GovApprovalLatencyP90,
	GovForbiddenAttemptCount,
}

// DataStatus is the per-metric availability state. "Unmeasured" is never
// disguised as zero: unavailable carries a machine-readable reason.
type DataStatus string

const (
	StatusReady          DataStatus = "ready"
	StatusEmpty          DataStatus = "empty"
	StatusUnavailable    DataStatus = "unavailable"
	StatusNotApplicable  DataStatus = "not_applicable"
)

// Attribution records how many raw rows were attributed to a user vs not
// (SDD §2.2). Coverage is nil when not computable.
type Attribution struct {
	AttributedCount   int64    `json:"attributed_count"`
	UnattributedCount int64    `json:"unattributed_count"`
	Coverage          *float64 `json:"coverage"`
}

// MetricValue is the frozen raw value of one metric for one snapshot row
// (SDD §2.2). Value/Num/Denom/Reason are pointers so JSON null is expressible.
type MetricValue struct {
	Value       *float64     `json:"value"`
	Numerator   *float64     `json:"numerator"`
	Denominator *float64     `json:"denominator"`
	Unit        string       `json:"unit"`
	DataStatus  DataStatus   `json:"data_status"`
	Reason      *string      `json:"reason"`
	Attribution *Attribution `json:"attribution"`
}

// Headline carries the org-level summary counters.
type Headline struct {
	ActiveMembers int64    `json:"active_members"`
	TotalTokens   int64    `json:"total_tokens"`
	CostUSD       *float64 `json:"cost_usd"`
	CostStatus    string   `json:"cost_status"`
}

// SnapshotMetricsV1 is the frozen metrics payload of one snapshot row.
type SnapshotMetricsV1 struct {
	Schema       string                                `json:"schema"`
	Headline     Headline                              `json:"headline"`
	MetricValues map[MetricKey]MetricValue             `json:"metric_values"`
	Governance   map[GovernanceMetricKey]MetricValue   `json:"governance"`
}

// SnapshotScoresV1 is the frozen scoring payload. During observation (or when
// any scoring metric is not ready) the stored value is the empty JSON object.
type SnapshotScoresV1 struct {
	Schema          string                    `json:"schema"`
	MetricScores    map[MetricKey]float64     `json:"metric_scores"`
	DimensionScores map[DimensionKey]float64  `json:"dimension_scores"`
	TotalScore      float64                   `json:"total_score"`
}

// MetricConfig is one metric's weight/floor/target (SDD §2.4).
type MetricConfig struct {
	Weight float64 `json:"weight"`
	Floor  float64 `json:"floor"`
	Target float64 `json:"target"`
}

// ConfigV1 mirrors maturity-config.yaml. CalibrationStatus can only move to
// "calibrated" after CR-D human review.
type ConfigV1 struct {
	Schema            string                      `json:"schema"`
	ObservationWeeks  int                         `json:"observation_weeks"`
	CalibrationStatus string                      `json:"calibration_status"`
	Dimensions        map[DimensionKey][]MetricKey `json:"dimensions"`
	Metrics           map[MetricKey]MetricConfig  `json:"metrics"`
}

const configSchema = "ai-first.maturity-config/v1"

var canonicalDimensions = map[DimensionKey][]MetricKey{
	DimAIF: {MetricTokenIntensity, MetricAIPenetration},
	DimSII: {MetricCRThroughputPerCapita},
	DimOFI: {MetricProjectCollabScale, MetricProjectActiveRate},
	DimEPC: {MetricPrototypeDirectRate},
	DimACM: {MetricTeamAgentDepth, MetricProcessCompletionRate},
}

// ValidateConfig enforces the generator's hard rules on a ConfigV1 value
// (SDD §2.4). It exists so a future CR-D "calibrated" declaration is rejected
// at load time unless every rule holds.
func ValidateConfig(c ConfigV1) error {
	if c.Schema != configSchema {
		return fmt.Errorf("schema must be %q, got %q", configSchema, c.Schema)
	}
	if c.ObservationWeeks != 4 {
		return fmt.Errorf("observation_weeks must be 4, got %d", c.ObservationWeeks)
	}
	if c.CalibrationStatus != "observing" && c.CalibrationStatus != "calibrated" {
		return fmt.Errorf("calibration_status must be observing|calibrated, got %q", c.CalibrationStatus)
	}
	if len(c.Metrics) != len(AllMetricKeys) {
		return fmt.Errorf("metrics must have exactly %d keys, got %d", len(AllMetricKeys), len(c.Metrics))
	}
	sum := 0.0
	for _, k := range AllMetricKeys {
		mc, ok := c.Metrics[k]
		if !ok {
			return fmt.Errorf("missing metric key %q", k)
		}
		if !(mc.Weight > 0 && mc.Weight <= 1) {
			return fmt.Errorf("metric %s: weight must be in (0,1], got %v", k, mc.Weight)
		}
		if !(mc.Target > mc.Floor) {
			return fmt.Errorf("metric %s: target must be > floor (%v > %v)", k, mc.Target, mc.Floor)
		}
		sum += mc.Weight
	}
	if math.Abs(sum-1) > 1e-9 {
		return fmt.Errorf("metric weights must sum to 1, got %v", sum)
	}
	if len(c.Dimensions) != len(canonicalDimensions) {
		return fmt.Errorf("dimensions must have exactly %d keys, got %d", len(canonicalDimensions), len(c.Dimensions))
	}
	for d, want := range canonicalDimensions {
		got, ok := c.Dimensions[d]
		if !ok {
			return fmt.Errorf("missing dimension %q", d)
		}
		if len(got) != len(want) {
			return fmt.Errorf("dimension %s: expected %d metrics, got %d", d, len(want), len(got))
		}
		for i := range want {
			if got[i] != want[i] {
				return fmt.Errorf("dimension %s[%d]: expected %q, got %q", d, i, want[i], got[i])
			}
		}
	}
	return nil
}

// ModelPrice is the USD-per-1M-token price for one model (optional price map).
type ModelPrice struct {
	InputUSDPer1M     float64 `json:"input"`
	OutputUSDPer1M    float64 `json:"output"`
	CacheReadUSDPer1M float64 `json:"cache_read"`
	CacheWriteUSDPer1M float64 `json:"cache_write"`
}

// PriceMap is the generated copy of the optional model-prices.yaml.
type PriceMap struct {
	Models map[string]ModelPrice `json:"models"`
}
