package maturity

import "time"

// Read-path API types (SDD §3.2–§3.6). Shared by service and handler; the
// frontend mirrors these shapes in packages/core/api/schemas.ts.

// MaturityReport is the E3 weekly-report envelope stored in
// agent_task_queue.result (schema ai-first.maturity-report/v1).
type MaturityReport struct {
	ReportKey      string   `json:"report_key"`
	Week           string   `json:"week"`
	GeneratedAt    string   `json:"generated_at"`
	RelativePath   string   `json:"relative_path"`
	Markdown       string   `json:"markdown"`
	ContentSha256  string   `json:"content_sha256"`
	SourceTaskID   string   `json:"source_task_id"`
	ChatSessionID  string   `json:"chat_session_id"`
	ConfigRevs     []string `json:"config_revs"`
}

// Observation is the calibration/observation status block.
type Observation struct {
	Active            bool   `json:"active"`
	CalibrationStatus string `json:"calibration_status"`
	ObservationWeeks  int    `json:"observation_weeks"`
	FirstBucketDate   string `json:"first_bucket_date"`
	ElapsedDays       int    `json:"elapsed_days"`
}

// ApiError is the uniform 4xx payload.
type ApiError struct {
	Error     string `json:"error"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// MaturityOverallResponse mirrors GET /api/maturity/overall.
type MaturityOverallResponse struct {
	BucketDate   string       `json:"bucket_date"`
	ConfigRev    string       `json:"config_rev"`
	Observation  *Observation `json:"observation"`
	Headline     *Headline    `json:"headline"`
	TotalScore   *float64     `json:"total_score"`
	Dimensions   []MaturityDimension `json:"dimensions"`
	Governance   []MaturityGovernanceDatum `json:"governance"`
	DataStatus   string       `json:"data_status"`
}

// MaturityDimension groups metrics under one dimension key.
type MaturityDimension struct {
	Key       DimensionKey            `json:"key"`
	Score     *float64                `json:"score"`
	DataStatus DataStatus             `json:"data_status"`
	Metrics   []MaturityDimensionMetric `json:"metrics"`
}

// MaturityDimensionMetric is one raw+score pair inside a dimension.
type MaturityDimensionMetric struct {
	Key   MetricKey   `json:"key"`
	Raw   MetricValue `json:"raw"`
	Score *float64    `json:"score"`
}

// MaturityGovernanceDatum is one governance guardrail value.
type MaturityGovernanceDatum struct {
	Key   GovernanceMetricKey `json:"key"`
	Datum MetricValue         `json:"datum"`
}

// TokenTrendQuery is the parsed token-trend request.
type TokenTrendQuery struct {
	Dimension   string
	DimensionID string
	From        time.Time
	To          time.Time
	IncludeCost bool
}

// TokenTrendPoint is one dated point in a trend series.
type TokenTrendPoint struct {
	Date       string   `json:"date"`
	Tokens     int64    `json:"tokens"`
	CostUSD    *float64 `json:"cost_usd"`
	CostStatus string   `json:"cost_status"`
}

// TokenTrendSeries is one series (project, self user, or model).
type TokenTrendSeries struct {
	ID     string            `json:"id"`
	Label  string            `json:"label"`
	Points []TokenTrendPoint `json:"points"`
}

// TokenTrendResponse mirrors GET /api/maturity/token-trend.
type TokenTrendResponse struct {
	Dimension  string              `json:"dimension"`
	From       string              `json:"from"`
	To         string              `json:"to"`
	Series     []TokenTrendSeries  `json:"series"`
	DataStatus string              `json:"data_status"`
}

// ProjectRankingsItem is one row of the project ranking.
type ProjectRankingsItem struct {
	Rank        int        `json:"rank"`
	ProjectID   string     `json:"project_id"`
	ProjectName string     `json:"project_name"`
	Value       *float64   `json:"value"`
	DataStatus  DataStatus `json:"data_status"`
}

// ProjectRankingsResponse mirrors GET /api/maturity/rankings.
type ProjectRankingsResponse struct {
	Scope      string                `json:"scope"`
	BucketDate string                `json:"bucket_date"`
	Metric     string                `json:"metric"`
	Items      []ProjectRankingsItem `json:"items"`
	NextCursor string                `json:"next_cursor"`
	DataStatus string                `json:"data_status"`
}

// SuggestionResponse mirrors GET /api/maturity/suggestions.
type SuggestionResponse struct {
	Latest     *MaturityReport `json:"latest"`
	DataStatus string          `json:"data_status"`
}

// SuggestionHistoryResponse mirrors GET /api/maturity/suggestions/history.
type SuggestionHistoryResponse struct {
	Items      []MaturityReport `json:"items"`
	NextCursor string           `json:"next_cursor"`
	DataStatus string           `json:"data_status"`
}

// MaturityConfigMetric is one metric row of GET /api/maturity/config.
type MaturityConfigMetric struct {
	Key             MetricKey `json:"key"`
	Weight          float64   `json:"weight"`
	Floor           float64   `json:"floor"`
	Target          float64   `json:"target"`
	Unit            string    `json:"unit"`
	KnownGameability string   `json:"known_gameability"`
}

// MaturityConfigDimension is one dimension row of the config response.
type MaturityConfigDimension struct {
	Key     DimensionKey `json:"key"`
	Metrics []MetricKey  `json:"metrics"`
}

// MaturityConfigResponse mirrors GET /api/maturity/config.
type MaturityConfigResponse struct {
	ConfigRev         string                   `json:"config_rev"`
	ObservationWeeks  int                      `json:"observation_weeks"`
	CalibrationStatus string                   `json:"calibration_status"`
	Dimensions        []MaturityConfigDimension `json:"dimensions"`
	Metrics           []MaturityConfigMetric   `json:"metrics"`
	PriceConfigRev    *string                  `json:"price_config_rev"`
}
