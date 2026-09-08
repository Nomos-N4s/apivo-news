// What a deployment says about its telemetry (ADR-0007).
//
// A file of its own rather than three more fields on Config, for the reason
// networks.go is a file of its own: these keys travel together, they are read
// by one package, and a deployment that sets none of them is in a supported
// state that deserves saying once rather than being inferred three times.

package config

import (
	"fmt"
	"strconv"
	"strings"
)

// TelemetryKey is the environment variable that switches telemetry on. It is
// OpenTelemetry's own standard name rather than an APIVO_ one, so an operator
// who knows the ecosystem does not have to learn ours, and so a future
// sidecar reading the same convention agrees with us by default.
const TelemetryKey = "OTEL_EXPORTER_OTLP_ENDPOINT"

// TelemetryConfig is the collector this deployment exports to, and how.
//
// EMPTY IS A STATE, not a defect: a deployment with no endpoint runs, says so
// once at start-up, and emits nothing. Observability is not availability
// (ADR-0007), and the constitution now requires that a sidecar which is not
// needed to answer a request never be on the path of one.
type TelemetryConfig struct {
	// Endpoint is the OTLP/HTTP collector root, e.g.
	// http://apivo-qa-alloy:4318. Empty means telemetry is off.
	Endpoint string
	// ServiceName identifies this process to every backend. Empty means the
	// telemetry package's own default.
	ServiceName string
	// SampleRatio is the fraction of root traces recorded, 0..1. Empty means
	// the package default, which records all of them - the right answer at
	// this deployment's volume, and one to revisit rather than tune blind.
	SampleRatio float64
}

// Enabled reports whether this deployment configured a collector.
func (t TelemetryConfig) Enabled() bool { return strings.TrimSpace(t.Endpoint) != "" }

// LogValue renders the configuration for a log line.
//
// There is nothing secret here - an endpoint is a container name on an
// internal network, not a credential - so this exists for readability rather
// than for redaction, and says the one thing an operator reading start-up
// output wants: is it on.
func (t TelemetryConfig) LogValue() any {
	return struct {
		Enabled     bool   `json:"enabled"`
		Endpoint    string `json:"endpoint"`
		ServiceName string `json:"service_name"`
	}{t.Enabled(), t.Endpoint, t.ServiceName}
}

// parseTelemetry reads the telemetry block.
//
// The ratio is the only value that can be WRONG rather than merely absent,
// and it is refused rather than clamped: a deployment that asked for 2.0 or
// for -1 meant something, and quietly recording everything or nothing instead
// would be answering a question nobody asked.
func parseTelemetry(getenv func(string) string) (TelemetryConfig, error) {
	t := TelemetryConfig{
		Endpoint:    strings.TrimSpace(getenv(TelemetryKey)),
		ServiceName: strings.TrimSpace(getenv("OTEL_SERVICE_NAME")),
	}
	raw := strings.TrimSpace(getenv("TELEMETRY_SAMPLE_RATIO"))
	if raw == "" {
		return t, nil
	}
	ratio, err := strconv.ParseFloat(raw, 64)
	if err != nil || ratio < 0 || ratio > 1 {
		return TelemetryConfig{}, fmt.Errorf("config: TELEMETRY_SAMPLE_RATIO must be a number between 0 and 1, got %q", raw)
	}
	t.SampleRatio = ratio
	return t, nil
}
