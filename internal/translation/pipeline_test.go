package translation

// Unit tests for the pipeline's construction-time validation and its
// failure classification. The cycle itself runs against the real schema -
// claims, budget stops and ledger movement are integration territory
// (pipeline_integration_test.go).

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"
)

// validPipelineConfig is a configuration NewPipeline has no objection to.
func validPipelineConfig() PipelineConfig {
	return PipelineConfig{
		Interval:      time.Minute,
		ReaderLocales: []string{"el", "de"},
		Caps:          Caps{PerArticleMicroUSD: 20_000, MonthlyMicroUSD: 25_000_000},
	}
}

func TestNewPipelineRefusesAConfigurationItCannotRun(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)

	tests := []struct {
		name   string
		mutate func(*PipelineConfig)
	}{
		// A missing budget is not an unlimited one: a pipeline without
		// caps must never reach a provider.
		{"no caps", func(c *PipelineConfig) { c.Caps = Caps{} }},
		// An unregistered prompt version would be recorded as the lineage
		// of every row written with it - and would be a lie.
		{"unknown prompt version", func(c *PipelineConfig) { c.PromptVersion = "v0-never-released" }},
		// No reader locales means no work could ever be eligible; running
		// anyway would poll the database every interval for nothing.
		{"no reader locales", func(c *PipelineConfig) { c.ReaderLocales = nil }},
		// Interval zero means "disabled", and a disabled pipeline is one
		// the composition root never constructs; accepting it here would
		// produce a loop that spins hot.
		{"zero interval", func(c *PipelineConfig) { c.Interval = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validPipelineConfig()
			tt.mutate(&cfg)
			if _, err := NewPipeline(log, nil, nil, cfg); err == nil {
				t.Fatalf("NewPipeline() accepted a configuration it cannot run: %+v", cfg)
			}
		})
	}

	t.Run("valid configuration defaults the rest", func(t *testing.T) {
		t.Parallel()
		p, err := NewPipeline(log, nil, nil, validPipelineConfig())
		if err != nil {
			t.Fatalf("NewPipeline() = %v, want nil", err)
		}
		if p.cfg.Limit != DefaultCycleLimit {
			t.Errorf("Limit defaulted to %d, want %d", p.cfg.Limit, DefaultCycleLimit)
		}
		if p.cfg.PromptVersion != CurrentPromptVersion {
			t.Errorf("PromptVersion defaulted to %q, want %q", p.cfg.PromptVersion, CurrentPromptVersion)
		}
	})
}

// TestFailureClassification pins which failures step to the next item and
// which end the cycle: misreading an item failure as provider-wide stops
// every translation over one bad feed entry, while the reverse pays the
// adapter's whole retry budget once per item against a host that is down.
func TestFailureClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          error
		wantItem     bool
		wantProvider bool
	}{
		{"invalid request", ErrInvalidRequest, true, false},
		{"invalid response", ErrInvalidResponse, true, false},
		{"timeout", ErrTimeout, true, false},
		{"auth", ErrAuth, false, true},
		{"rate limited", ErrRateLimited, false, true},
		{"unavailable", ErrUnavailable, false, true},
		// A SpendError classifies by what it wraps: the money is booked
		// separately, the verdict comes from the failure itself.
		{"spend error wrapping a response failure", &SpendError{Spend: Spend{CostMicroUSD: 5}, Err: ErrInvalidResponse}, true, false},
		{"spend error wrapping a rate limit", &SpendError{Spend: Spend{CostMicroUSD: 5}, Err: fmt.Errorf("wrapped: %w", ErrRateLimited)}, false, true},
		// Anything unclassified is infrastructure and fails the cycle.
		{"plain database error", errors.New("connection refused"), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := itemFailure(tt.err); got != tt.wantItem {
				t.Errorf("itemFailure(%v) = %t, want %t", tt.err, got, tt.wantItem)
			}
			if got := providerFailure(tt.err); got != tt.wantProvider {
				t.Errorf("providerFailure(%v) = %t, want %t", tt.err, got, tt.wantProvider)
			}
		})
	}
}
