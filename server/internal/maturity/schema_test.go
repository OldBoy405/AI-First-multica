package maturity

import (
	"regexp"
	"testing"
)

func validConfig() ConfigV1 {
	c := ConfigV1{
		Schema:            "ai-first.maturity-config/v1",
		ObservationWeeks:  4,
		CalibrationStatus: "observing",
		Dimensions: map[DimensionKey][]MetricKey{
			DimAIF: {MetricTokenIntensity, MetricAIPenetration},
			DimSII: {MetricCRThroughputPerCapita},
			DimOFI: {MetricProjectCollabScale, MetricProjectActiveRate},
			DimEPC: {MetricPrototypeDirectRate},
			DimACM: {MetricTeamAgentDepth, MetricProcessCompletionRate},
		},
		Metrics: map[MetricKey]MetricConfig{},
	}
	for _, k := range AllMetricKeys {
		c.Metrics[k] = MetricConfig{Weight: 0.125, Floor: 0, Target: 1}
	}
	return c
}

func TestValidateConfig(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ConfigV1)
		wantErr string
	}{
		{"valid", func(*ConfigV1) {}, ""},
		{"bad schema", func(c *ConfigV1) { c.Schema = "x" }, "schema"},
		{"weeks", func(c *ConfigV1) { c.ObservationWeeks = 5 }, "observation_weeks"},
		{"status", func(c *ConfigV1) { c.CalibrationStatus = "ready" }, "calibration_status"},
		{"missing metric", func(c *ConfigV1) { delete(c.Metrics, MetricTeamAgentDepth) }, "metrics must have exactly"},
		{"unknown metric", func(c *ConfigV1) { c.Metrics["extra"] = MetricConfig{Weight: 0.125, Floor: 0, Target: 1} }, "metrics must have exactly"},
		{"weight zero", func(c *ConfigV1) { m := c.Metrics[MetricAIPenetration]; m.Weight = 0; c.Metrics[MetricAIPenetration] = m }, "weight"},
		{"weight over 1", func(c *ConfigV1) { m := c.Metrics[MetricAIPenetration]; m.Weight = 1.5; c.Metrics[MetricAIPenetration] = m }, "weight"},
		{"weight sum", func(c *ConfigV1) { m := c.Metrics[MetricTokenIntensity]; m.Weight = 0.5; c.Metrics[MetricTokenIntensity] = m }, "sum to 1"},
		{"floor equals target", func(c *ConfigV1) { m := c.Metrics[MetricTokenIntensity]; m.Floor = 1; c.Metrics[MetricTokenIntensity] = m }, "target"},
		{"missing dimension", func(c *ConfigV1) { delete(c.Dimensions, DimEPC) }, "dimension"},
		{"dimension metric swapped", func(c *ConfigV1) {
			c.Dimensions[DimAIF] = []MetricKey{MetricAIPenetration, MetricTokenIntensity}
		}, "dimension"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(&c)
			err := ValidateConfig(c)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !regexp.MustCompile(tc.wantErr).MatchString(err.Error()) {
				t.Fatalf("error %q does not match %q", err, tc.wantErr)
			}
		})
	}
}

func TestGeneratedConfig(t *testing.T) {
	c := GeneratedConfig()
	if err := ValidateConfig(c); err != nil {
		t.Fatalf("generated config must validate: %v", err)
	}
	if c.CalibrationStatus != "observing" {
		t.Fatalf("CR-A initial seed must be observing, got %q", c.CalibrationStatus)
	}
	for _, k := range AllMetricKeys {
		mc := c.Metrics[k]
		if mc.Weight != 0.125 || mc.Floor != 0 || mc.Target != 1 {
			t.Fatalf("metric %s seed must be weight=0.125,floor=0,target=1, got %+v", k, mc)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(GeneratedConfigRev()) {
		t.Fatalf("GeneratedConfigRev must be 40-hex, got %q", GeneratedConfigRev())
	}
	if _, ok := GeneratedPriceMap(); ok {
		t.Fatal("no model-prices.yaml is committed; price map must report absent")
	}
}
