package main

// Contract tests for serve()'s translation arms, mirroring the editorial
// wiring tests' style: the pipeline starts only on a complete
// TRANSLATION_* configuration, stays off - loudly, and in the right
// register - on an absent or incomplete one, and a configuration that is
// complete but wrong fails startup, because an operator who set nine keys
// believes translation is on.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// runServing starts run() with the given environment, waits until it
// serves, and returns its log buffer plus a stop function that cancels
// and requires a clean exit.
func runServing(t *testing.T, env map[string]string) (*syncBuffer, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- run(ctx, nil, envFrom(env), out) }()

	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(out.String(), "starting") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("run() never reached the serving phase; output: %q", out.String())
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("run() exited before serving: %v; output: %q", err, out.String())
		case <-time.After(25 * time.Millisecond):
		}
	}
	return out, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("run() after cancel: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("run() did not return after context cancellation")
		}
	}
}

// isolatedDatabase creates a throwaway database beside the configured one
// and returns a connection URL for it. The pipeline-started test runs a
// real first cycle immediately, and it must find NO eligible items: this
// test asserts the wiring, and translating - or even briefly claiming -
// another suite's seeded work would be interference, not coverage.
func isolatedDatabase(t *testing.T, baseURL string) string {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("connecting to create an isolated database: %v", err)
	}
	t.Cleanup(pool.Close)

	name := "wiring_translation_" + randomHex(t)
	if _, err := pool.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("creating the isolated database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Logf("dropping the isolated database %s: %v", name, err)
		}
	})

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parsing DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// fullTranslationEnv is a complete TRANSLATION_* configuration pointing at
// the given provider base URL.
func fullTranslationEnv(dbURL, providerURL string) map[string]string {
	return map[string]string{
		"DATABASE_URL":  dbURL,
		"HTTP_ADDR":     "127.0.0.1:0",
		"POLL_INTERVAL": "0",

		"TRANSLATION_BASE_URL":                 providerURL,
		"TRANSLATION_MODEL":                    "wiring-test-model",
		"TRANSLATION_FREE_OF_CHARGE":           "true",
		"TRANSLATION_ARTICLE_CEILING_MICROUSD": "20000",
		"TRANSLATION_MONTHLY_CAP_MICROUSD":     "25000000",
		"TRANSLATION_INTERVAL":                 "1h",
	}
}

// TestRunStartsTheTranslationPipelineWhenConfigured proves serve() wires
// and starts the pipeline on a complete configuration: the startup line
// names the model and budget, and the process still shuts down cleanly
// with the pipeline goroutine running.
func TestRunStartsTheTranslationPipelineWhenConfigured(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	// Never called - the isolated database holds no eligible items - but
	// answering 401 keeps even a surprise call cheap, fast and free.
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(provider.Close)

	out, stop := runServing(t, fullTranslationEnv(isolatedDatabase(t, dbURL), provider.URL+"/v1"))
	defer stop()

	logged := out.String()
	if !strings.Contains(logged, "translation pipeline started") {
		t.Errorf("serve() with a complete configuration never announced the pipeline; output: %q", logged)
	}
	if !strings.Contains(logged, "wiring-test-model") {
		t.Errorf("the startup line does not name the configured model; output: %q", logged)
	}
}

// TestRunServesWithTheTranslationPipelineOff pins the three off states and
// their registers: absence is an expected state and says so quietly,
// half a configuration is a mistake and says so at ERROR, and
// TRANSLATION_INTERVAL=0 is the one deliberate disable switch. All three
// serve - a translation misconfiguration must never take the public site
// down - and all three name the consequence in one line.
func TestRunServesWithTheTranslationPipelineOff(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	tests := []struct {
		name    string
		env     map[string]string
		wantLog []string
	}{
		{
			name: "absent configuration is OFF, quietly, naming what to set",
			env: map[string]string{
				"DATABASE_URL":  dbURL,
				"HTTP_ADDR":     "127.0.0.1:0",
				"POLL_INTERVAL": "0",
			},
			wantLog: []string{
				"level=INFO",
				"the translation pipeline is OFF",
				"TRANSLATION_BASE_URL",
				"TRANSLATION_MONTHLY_CAP_MICROUSD",
			},
		},
		{
			name: "incomplete configuration is OFF, at ERROR, naming what is missing",
			env: map[string]string{
				"DATABASE_URL":      dbURL,
				"HTTP_ADDR":         "127.0.0.1:0",
				"POLL_INTERVAL":     "0",
				"TRANSLATION_MODEL": "half-configured-model",
			},
			wantLog: []string{
				"level=ERROR",
				"INCOMPLETE",
				"TRANSLATION_BASE_URL",
				"TRANSLATION_MONTHLY_CAP_MICROUSD",
			},
		},
		{
			name: "interval zero is the deliberate disable switch",
			env: func() map[string]string {
				env := fullTranslationEnv(dbURL, "http://127.0.0.1:9/v1")
				env["TRANSLATION_INTERVAL"] = "0"
				return env
			}(),
			wantLog: []string{
				"level=INFO",
				"TRANSLATION_INTERVAL is 0",
				"no item will be translated",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, stop := runServing(t, tt.env)
			defer stop()
			logged := out.String()
			if strings.Contains(logged, "translation pipeline started") {
				t.Errorf("the pipeline started despite the configuration saying off; output: %q", logged)
			}
			for _, want := range tt.wantLog {
				if !strings.Contains(logged, want) {
					t.Errorf("startup log does not contain %q; output: %q", want, logged)
				}
			}
		})
	}
}

// TestRunFailsStartupOnMalformedTranslationValues proves a TRANSLATION_*
// value that is set but unparseable fails startup at configuration time -
// before any database or provider is touched, so no running database is
// needed - rather than silently disabling a pipeline the operator
// believes is on.
func TestRunFailsStartupOnMalformedTranslationValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		key, value  string
		wantInError string
	}{
		{"price is not a number", "TRANSLATION_INPUT_USD_PER_MTOK", "free", "TRANSLATION_INPUT_USD_PER_MTOK"},
		{"zero price imitating free", "TRANSLATION_OUTPUT_USD_PER_MTOK", "0", "TRANSLATION_FREE_OF_CHARGE"},
		{"zero monthly cap", "TRANSLATION_MONTHLY_CAP_MICROUSD", "0", "TRANSLATION_MONTHLY_CAP_MICROUSD"},
		{"negative article ceiling", "TRANSLATION_ARTICLE_CEILING_MICROUSD", "-5", "TRANSLATION_ARTICLE_CEILING_MICROUSD"},
		{"interval is not a duration", "TRANSLATION_INTERVAL", "soon", "TRANSLATION_INTERVAL"},
		{"free-of-charge is not a boolean", "TRANSLATION_FREE_OF_CHARGE", "kind of", "TRANSLATION_FREE_OF_CHARGE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{
				// Never dialled: the configuration fails first.
				"DATABASE_URL": "postgres://unused.invalid/apivo",
				tt.key:         tt.value,
			}
			err := run(context.Background(), nil, envFrom(env), io.Discard)
			if err == nil {
				t.Fatalf("run() with %s=%q: want a startup error, got nil", tt.key, tt.value)
			}
			if !strings.Contains(err.Error(), tt.wantInError) {
				t.Errorf("run() error %q does not name %q", err, tt.wantInError)
			}
		})
	}
}

// TestRunFailsStartupOnACompleteButUnusableTranslation proves the other
// half of the fail-fast stance: a configuration that parses but cannot be
// wired - a base URL the adapter refuses, a budget whose ceiling exceeds
// its own monthly cap - stops the process instead of serving with a
// pipeline that could never run as configured.
func TestRunFailsStartupOnACompleteButUnusableTranslation(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise this test")
	}

	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"unusable base URL", func(env map[string]string) {
			env["TRANSLATION_BASE_URL"] = "::not-a-url::"
		}},
		{"ceiling above the monthly cap", func(env map[string]string) {
			env["TRANSLATION_ARTICLE_CEILING_MICROUSD"] = "50000000"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := fullTranslationEnv(dbURL, "http://127.0.0.1:9/v1")
			tt.mutate(env)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := run(ctx, nil, envFrom(env), io.Discard); err == nil {
				t.Fatal("run() with an unusable translation configuration: want error, got nil")
			}
		})
	}
}
