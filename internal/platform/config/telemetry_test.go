package config_test

// What a deployment says about its telemetry (ADR-0007).

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
)

// TestTelemetryIsOffUntilAnEndpointNamesACollector. Off is a state, not a
// defect: the api serves, and says so once. Asserted because every other
// suite in this repository runs in exactly this configuration.
func TestTelemetryIsOffUntilAnEndpointNamesACollector(t *testing.T) {
	t.Parallel()
	cfg, err := config.FromEnv(envFrom(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatalf("a deployment with no telemetry must configure: %v", err)
	}
	if cfg.Telemetry.Enabled() {
		t.Error("telemetry reports itself enabled with no endpoint set")
	}
}

// TestAnEndpointSwitchesTelemetryOn.
func TestAnEndpointSwitchesTelemetryOn(t *testing.T) {
	t.Parallel()
	cfg, err := config.FromEnv(envFrom(map[string]string{
		"DATABASE_URL":      "postgres://x",
		config.TelemetryKey: "http://apivo-qa-alloy:4318",
		"OTEL_SERVICE_NAME": "apivo-api-qa",
	}))
	if err != nil {
		t.Fatalf("configuring: %v", err)
	}
	if !cfg.Telemetry.Enabled() {
		t.Fatal("telemetry is off with an endpoint set")
	}
	if cfg.Telemetry.ServiceName != "apivo-api-qa" {
		t.Errorf("service name = %q, want %q", cfg.Telemetry.ServiceName, "apivo-api-qa")
	}
}

// TestWhitespaceIsNotAnEndpoint. A key set to spaces in an env file is the
// operator's finger, not their intent, and reading it as configured would
// start a deployment that exports to nowhere while reporting itself on.
func TestWhitespaceIsNotAnEndpoint(t *testing.T) {
	t.Parallel()
	cfg, err := config.FromEnv(envFrom(map[string]string{"DATABASE_URL": "postgres://x", config.TelemetryKey: "   "}))
	if err != nil {
		t.Fatalf("configuring: %v", err)
	}
	if cfg.Telemetry.Enabled() {
		t.Error("whitespace read as a configured collector")
	}
}

// TestAnImpossibleSampleRatioIsRefusedRatherThanClamped. A deployment that
// asked for 2.0 or -1 meant something. Clamping would answer a question
// nobody asked, and silently recording everything or nothing is exactly the
// failure that makes somebody distrust the traces they do have.
func TestAnImpossibleSampleRatioIsRefusedRatherThanClamped(t *testing.T) {
	t.Parallel()
	for _, ratio := range []string{"2", "-1", "1.5", "half", "", " "} {
		if ratio == "" || strings.TrimSpace(ratio) == "" {
			// Empty means "use the default", which is not an error.
			cfg, err := config.FromEnv(envFrom(map[string]string{"DATABASE_URL": "postgres://x", "TELEMETRY_SAMPLE_RATIO": ratio}))
			if err != nil {
				t.Errorf("TELEMETRY_SAMPLE_RATIO=%q was refused; empty means the default", ratio)
			}
			if cfg.Telemetry.SampleRatio != 0 {
				t.Errorf("TELEMETRY_SAMPLE_RATIO=%q parsed to %v, want the zero that means default", ratio, cfg.Telemetry.SampleRatio)
			}
			continue
		}
		if _, err := config.FromEnv(envFrom(map[string]string{"DATABASE_URL": "postgres://x", "TELEMETRY_SAMPLE_RATIO": ratio})); err == nil {
			t.Errorf("TELEMETRY_SAMPLE_RATIO=%q was accepted; it is not a ratio between 0 and 1", ratio)
		}
	}
}

// TestAValidSampleRatioIsKept, including the two ends.
func TestAValidSampleRatioIsKept(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]float64{"0": 0, "0.25": 0.25, "1": 1} {
		cfg, err := config.FromEnv(envFrom(map[string]string{"DATABASE_URL": "postgres://x", "TELEMETRY_SAMPLE_RATIO": raw}))
		if err != nil {
			t.Fatalf("TELEMETRY_SAMPLE_RATIO=%q: %v", raw, err)
		}
		if cfg.Telemetry.SampleRatio != want {
			t.Errorf("TELEMETRY_SAMPLE_RATIO=%q parsed to %v, want %v", raw, cfg.Telemetry.SampleRatio, want)
		}
	}
}

// TestTheLogValueSaysWhetherItIsOn. There is no secret here - an endpoint is
// a container name on an internal network - so this exists for the operator
// reading start-up output, and the one thing they want is whether it is on.
func TestTheLogValueSaysWhetherItIsOn(t *testing.T) {
	t.Parallel()
	cfg, err := config.FromEnv(envFrom(map[string]string{"DATABASE_URL": "postgres://x", config.TelemetryKey: "http://alloy:4318"}))
	if err != nil {
		t.Fatalf("configuring: %v", err)
	}
	rendered := cfg.Telemetry.LogValue()
	if rendered == nil {
		t.Fatal("LogValue rendered nothing")
	}
	if got := fmt.Sprintf("%+v", rendered); !strings.Contains(got, "alloy") || !strings.Contains(got, "true") {
		t.Errorf("LogValue = %s, want it to name the collector and say it is on", got)
	}
}
