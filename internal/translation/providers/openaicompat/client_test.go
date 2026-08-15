package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/translation"
)

// The item every test translates. Greek to German is the alpha pair.
var sampleRequest = translation.Request{
	SourceTitle:    "Δήμαρχος ανακοινώνει νέο πάρκο",
	SourceText:     "Το νέο πάρκο ανοίγει τον Μάιο στο κέντρο της πόλης.",
	SourceLanguage: "el",
	TargetLanguage: "de",
	PromptVersion:  translation.CurrentPromptVersion,
}

const (
	wantHeadline = "Bürgermeister kündigt neuen Park an"
	wantExtract  = "Der neue Park öffnet im Mai im Stadtzentrum."
	hostModelID  = "host/qwen3.5-9b"
)

// translationJSON is what a well-behaved model returns as its content.
func translationJSON(headline, extract string) string {
	payload, err := json.Marshal(translated{Headline: headline, Extract: extract})
	if err != nil {
		panic(err)
	}
	return string(payload)
}

// completion renders a 200 chat-completions body carrying the given
// content and token usage.
func completion(content string, promptTokens, completionTokens int64) string {
	return completionWith(map[string]any{
		"model": hostModelID,
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
		},
	})
}

// completionWith renders an arbitrary response body, for the shapes a
// well-formed completion cannot express.
func completionWith(body map[string]any) string {
	payload, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(payload)
}

// usageShapeHandler answers with a valid translation and the given usage
// block verbatim, for the shapes a well-formed usage struct cannot
// express: totals only, empty, another vendor's field names, or null.
func usageShapeHandler(content string, reported map[string]any) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{
			"model": hostModelID,
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": content},
				"finish_reason": "stop",
			}},
			"usage": reported,
		}
		answerJSON(w, http.StatusOK, completionWith(body))
	}
}

// recordedCall is what the fake host saw.
type recordedCall struct {
	method      string
	path        string
	authz       string
	contentType string
	accept      string
	body        []byte
}

// fakeHost is an httptest server that hands its handler the attempt number
// and records every call, so a test can assert on both the request shape
// and how many attempts were made.
type fakeHost struct {
	*httptest.Server

	mu    sync.Mutex
	calls []recordedCall
}

func newFakeHost(t *testing.T, handler func(attempt int, w http.ResponseWriter, r *http.Request)) *fakeHost {
	t.Helper()

	host := &fakeHost{}
	host.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		host.mu.Lock()
		host.calls = append(host.calls, recordedCall{
			method:      r.Method,
			path:        r.URL.Path,
			authz:       r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
			accept:      r.Header.Get("Accept"),
			body:        body,
		})
		attempt := len(host.calls)
		host.mu.Unlock()

		handler(attempt, w, r)
	}))
	t.Cleanup(host.Close)
	return host
}

func (h *fakeHost) recorded() []recordedCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedCall(nil), h.calls...)
}

// answerJSON writes a chat-completions body with the given status.
func answerJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// testConfig is the base configuration every test starts from: prices are
// the 2026-08-14 Groq gpt-oss-120b snapshot, so the cost arithmetic in the
// tests is the arithmetic a real host would drive.
func testConfig(baseURL string) Config {
	return Config{
		BaseURL:                  baseURL + "/v1",
		Model:                    "configured-model",
		APIKey:                   "test-key-value",
		InputPricePerMillionUSD:  0.15,
		OutputPricePerMillionUSD: 0.60,
		MaxAttempts:              3,
		BaseBackoff:              10 * time.Millisecond,
		Timeout:                  2 * time.Second,
	}
}

// newTestClient builds a client whose backoff waits are recorded instead
// of slept, so retry behaviour is tested without spending real time.
func newTestClient(t *testing.T, cfg Config) (*Client, func() []time.Duration) {
	t.Helper()

	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	var (
		mu    sync.Mutex
		waits []time.Duration
	)
	client.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return ctx.Err()
	}
	return client, func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), waits...)
	}
}

func TestTranslate(t *testing.T) {
	t.Parallel()

	overLongExtract := strings.Repeat("α", translation.MaxExtractChars+1)
	goodContent := translationJSON(wantHeadline, wantExtract)

	tests := []struct {
		name string
		// handler answers the nth attempt.
		handler func(attempt int, w http.ResponseWriter, r *http.Request)
		// configure tweaks the base configuration.
		configure func(*Config)
		wantErr   error
		// wantErrText pins WHICH guard rejected the response, for the
		// cases that share a sentinel with several others.
		wantErrText string
		want        translation.Result
		wantCalls   int
		wantWaits   int
	}{
		{
			name: "plain json content",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completion(goodContent, 200, 160))
			},
			want: translation.Result{
				Headline:      wantHeadline,
				Extract:       wantExtract,
				Model:         hostModelID,
				PromptVersion: translation.CurrentPromptVersion,
				// 200 * 0.15 + 160 * 0.60 = 126 micro-USD.
				Spend: translation.Spend{CostMicroUSD: 126},
			},
			wantCalls: 1,
		},
		{
			name: "content wrapped in a code fence",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completion("```json\n"+goodContent+"\n```", 210, 150))
			},
			want: translation.Result{
				Headline:      wantHeadline,
				Extract:       wantExtract,
				Model:         hostModelID,
				PromptVersion: translation.CurrentPromptVersion,
				// 210 * 0.15 + 150 * 0.60 = 121.5 -> 122.
				Spend: translation.Spend{CostMicroUSD: 122},
			},
			wantCalls: 1,
		},
		{
			name: "content wrapped in prose and a fence",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				content := "Sure! Here is the translation:\n\n```json\n" + goodContent + "\n```\n\nLet me know if you need anything else."
				answerJSON(w, http.StatusOK, completion(content, 190, 170))
			},
			want: translation.Result{
				Headline:      wantHeadline,
				Extract:       wantExtract,
				Model:         hostModelID,
				PromptVersion: translation.CurrentPromptVersion,
				// 190 * 0.15 + 170 * 0.60 = 130.5 -> 131 (half away from zero).
				Spend: translation.Spend{CostMicroUSD: 131},
			},
			wantCalls: 1,
		},
		{
			name: "prose containing a decoy object before the answer",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				content := "Format {headline, extract} as follows:\n" + goodContent
				answerJSON(w, http.StatusOK, completion(content, 100, 100))
			},
			want: translation.Result{
				Headline:      wantHeadline,
				Extract:       wantExtract,
				Model:         hostModelID,
				PromptVersion: translation.CurrentPromptVersion,
				// 100 * 0.15 + 100 * 0.60 = 75.
				Spend: translation.Spend{CostMicroUSD: 75},
			},
			wantCalls: 1,
		},
		{
			name: "headline containing a brace survives parsing",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				content := translationJSON("Ein {seltsamer} Titel", wantExtract)
				answerJSON(w, http.StatusOK, completion(content, 120, 130))
			},
			want: translation.Result{
				Headline:      "Ein {seltsamer} Titel",
				Extract:       wantExtract,
				Model:         hostModelID,
				PromptVersion: translation.CurrentPromptVersion,
				// 120 * 0.15 + 130 * 0.60 = 96.
				Spend: translation.Spend{CostMicroUSD: 96},
			},
			wantCalls: 1,
		},
		{
			name: "host that does not echo the model",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completionWith(map[string]any{
					"choices": []any{map[string]any{
						"message": map[string]any{"content": goodContent},
					}},
					"usage": map[string]any{"prompt_tokens": 80, "completion_tokens": 60},
				}))
			},
			want: translation.Result{
				Headline:      wantHeadline,
				Extract:       wantExtract,
				Model:         "configured-model",
				PromptVersion: translation.CurrentPromptVersion,
				// 80 * 0.15 + 60 * 0.60 = 48.
				Spend: translation.Spend{CostMicroUSD: 48},
			},
			wantCalls: 1,
		},
		{
			name: "free host records an explicit zero cost",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completion(goodContent, 300, 240))
			},
			configure: func(c *Config) {
				c.InputPricePerMillionUSD = 0
				c.OutputPricePerMillionUSD = 0
				c.FreeOfCharge = true
			},
			want: translation.Result{
				Headline:      wantHeadline,
				Extract:       wantExtract,
				Model:         hostModelID,
				PromptVersion: translation.CurrentPromptVersion,
				Spend:         translation.Spend{CostMicroUSD: 0},
			},
			wantCalls: 1,
		},
		{
			name: "content is not json at all",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completion("Bürgermeister kündigt neuen Park an", 100, 40))
			},
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name: "content is malformed json",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completion(`{"headline": "Neuer Park", "extract":}`, 110, 45))
			},
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name: "content is empty",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completion("   ", 90, 0))
			},
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name: "content json is missing the extract",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completion(`{"headline": "Neuer Park"}`, 95, 30))
			},
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name: "extract over the character bound is refused",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completion(translationJSON(wantHeadline, overLongExtract), 250, 400))
			},
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			// The content is deliberately complete and valid JSON: only
			// the finish_reason guard can reject it, so the case cannot
			// pass by accident through the parser. An answer that hit the
			// cap is untrustworthy even when it happens to parse - the
			// model was still writing when it was cut off.
			name: "answer cut off at the output cap",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completionWith(map[string]any{
					"model": hostModelID,
					"choices": []any{map[string]any{
						"message":       map[string]any{"content": goodContent},
						"finish_reason": "length",
					}},
					"usage": map[string]any{"prompt_tokens": 200, "completion_tokens": 600},
				}))
			},
			wantErr:     translation.ErrInvalidResponse,
			wantErrText: "cut off at the output cap",
			wantCalls:   1,
		},
		{
			name: "response carries no choices",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completionWith(map[string]any{
					"model":   hostModelID,
					"choices": []any{},
					"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 0},
				}))
			},
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name: "response body is not json",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte("<html><body>gateway</body></html>"))
			},
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name: "missing usage block",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completionWith(map[string]any{
					"model": hostModelID,
					"choices": []any{map[string]any{
						"message":       map[string]any{"content": goodContent},
						"finish_reason": "stop",
					}},
				}))
			},
			wantErr:     translation.ErrInvalidResponse,
			wantErrText: "reports no token usage",
			wantCalls:   1,
		},
		{
			name:      "usage reporting totals only",
			handler:   usageShapeHandler(goodContent, map[string]any{"total_tokens": 365}),
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name:      "empty usage block",
			handler:   usageShapeHandler(goodContent, map[string]any{}),
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name:      "another vendor's usage names behind a gateway",
			handler:   usageShapeHandler(goodContent, map[string]any{"input_tokens": 210, "output_tokens": 155}),
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name:      "gemini-shaped usage names behind a gateway",
			handler:   usageShapeHandler(goodContent, map[string]any{"promptTokenCount": 210, "candidatesTokenCount": 155}),
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name:      "usage null",
			handler:   usageShapeHandler(goodContent, nil),
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name:      "zero prompt tokens on a priced host",
			handler:   usageShapeHandler(goodContent, map[string]any{"prompt_tokens": 0, "completion_tokens": 155}),
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name:      "zero completion tokens on a priced host",
			handler:   usageShapeHandler(goodContent, map[string]any{"prompt_tokens": 210, "completion_tokens": 0}),
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			// The counts are not load-bearing on a host that charges
			// nothing, so an odd usage shape is not a reason to throw a
			// translation away there.
			name:    "uninformative usage is tolerated on a free host",
			handler: usageShapeHandler(goodContent, map[string]any{"total_tokens": 365}),
			configure: func(c *Config) {
				c.InputPricePerMillionUSD, c.OutputPricePerMillionUSD = 0, 0
				c.FreeOfCharge = true
			},
			want: translation.Result{
				Headline:      wantHeadline,
				Extract:       wantExtract,
				Model:         hostModelID,
				PromptVersion: translation.CurrentPromptVersion,
				Spend:         translation.Spend{CostMicroUSD: 0},
			},
			wantCalls: 1,
		},
		{
			name: "negative token usage",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completion(goodContent, -5, 20))
			},
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name: "usage too large to price",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completion(goodContent, 9_000_000_000_000_000_000, 9_000_000_000_000_000_000))
			},
			configure: func(c *Config) {
				c.InputPricePerMillionUSD = 100
				c.OutputPricePerMillionUSD = 100
			},
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
		{
			name: "rejected credentials",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusUnauthorized, `{"error":{"message":"Invalid API Key"}}`)
			},
			wantErr:   translation.ErrAuth,
			wantCalls: 1,
		},
		{
			name: "forbidden model",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusForbidden, `{"error":{"message":"model not available on this plan"}}`)
			},
			wantErr:   translation.ErrAuth,
			wantCalls: 1,
		},
		{
			name: "unknown model is a request error",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusBadRequest, `{"error":{"message":"model_not_found"}}`)
			},
			wantErr:   translation.ErrInvalidRequest,
			wantCalls: 1,
		},
		{
			name: "rate limited then success",
			handler: func(attempt int, w http.ResponseWriter, _ *http.Request) {
				if attempt == 1 {
					answerJSON(w, http.StatusTooManyRequests, `{"error":{"message":"rate limit reached"}}`)
					return
				}
				answerJSON(w, http.StatusOK, completion(goodContent, 205, 155))
			},
			want: translation.Result{
				Headline:      wantHeadline,
				Extract:       wantExtract,
				Model:         hostModelID,
				PromptVersion: translation.CurrentPromptVersion,
				// 205 * 0.15 + 155 * 0.60 = 123.75 -> 124.
				Spend: translation.Spend{CostMicroUSD: 124},
			},
			wantCalls: 2,
			wantWaits: 1,
		},
		{
			name: "persistent rate limiting exhausts the budget",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusTooManyRequests, `{"error":{"message":"rate limit reached"}}`)
			},
			wantErr:   translation.ErrRateLimited,
			wantCalls: 3,
			wantWaits: 2,
		},
		{
			name: "host failure then success",
			handler: func(attempt int, w http.ResponseWriter, _ *http.Request) {
				if attempt == 1 {
					answerJSON(w, http.StatusBadGateway, `upstream unavailable`)
					return
				}
				answerJSON(w, http.StatusOK, completion(goodContent, 140, 90))
			},
			want: translation.Result{
				Headline:      wantHeadline,
				Extract:       wantExtract,
				Model:         hostModelID,
				PromptVersion: translation.CurrentPromptVersion,
				// 140 * 0.15 + 90 * 0.60 = 75.
				Spend: translation.Spend{CostMicroUSD: 75},
			},
			wantCalls: 2,
			wantWaits: 1,
		},
		{
			name: "persistent host failure",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusInternalServerError, `{"error":"internal"}`)
			},
			wantErr:   translation.ErrUnavailable,
			wantCalls: 3,
			wantWaits: 2,
		},
		{
			name: "a single attempt disables retrying",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusServiceUnavailable, `overloaded`)
			},
			configure: func(c *Config) { c.MaxAttempts = 1 },
			wantErr:   translation.ErrUnavailable,
			wantCalls: 1,
		},
		{
			name: "an unusable answer is never retried",
			handler: func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completion("not json", 50, 10))
			},
			wantErr:   translation.ErrInvalidResponse,
			wantCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost(t, tc.handler)
			cfg := testConfig(host.URL)
			if tc.configure != nil {
				tc.configure(&cfg)
			}
			client, waits := newTestClient(t, cfg)

			got, err := client.Translate(t.Context(), sampleRequest)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Translate() error = %v, want one matching %v", err, tc.wantErr)
				}
				if tc.wantErrText != "" && !strings.Contains(err.Error(), tc.wantErrText) {
					t.Errorf("Translate() error = %v, want the guard that says %q", err, tc.wantErrText)
				}
			} else {
				if err != nil {
					t.Fatalf("Translate() error = %v, want nil", err)
				}
				if got != tc.want {
					t.Errorf("Translate() = %+v, want %+v", got, tc.want)
				}
			}
			if calls := len(host.recorded()); calls != tc.wantCalls {
				t.Errorf("provider was called %d time(s), want %d", calls, tc.wantCalls)
			}
			if n := len(waits()); n != tc.wantWaits {
				t.Errorf("backed off %d time(s), want %d", n, tc.wantWaits)
			}
		})
	}
}

// TestRefusedAnswerReportsWhatItCost: a 200 the adapter throws away was
// still generated and billed. The cost has to reach the caller, or the
// monthly ledger drifts below the real total by everything the refusals
// cost (FR-006).
func TestRefusedAnswerReportsWhatItCost(t *testing.T) {
	t.Parallel()

	host := newFakeHost(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		answerJSON(w, http.StatusOK, completionWith(map[string]any{
			"model": hostModelID,
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": translationJSON(wantHeadline, wantExtract)},
				"finish_reason": "length",
			}},
			"usage": map[string]any{"prompt_tokens": 200, "completion_tokens": 160},
		}))
	})
	client, _ := newTestClient(t, testConfig(host.URL))

	_, err := client.Translate(t.Context(), sampleRequest)
	if !errors.Is(err, translation.ErrInvalidResponse) {
		t.Fatalf("Translate() error = %v, want ErrInvalidResponse", err)
	}

	var spent *translation.SpendError
	if !errors.As(err, &spent) {
		t.Fatalf("Translate() error = %v, want a *translation.SpendError carrying the discarded attempt's cost", err)
	}
	// 200 * 0.15 + 160 * 0.60 = 126 micro-USD, generated and billed.
	if spent.CostMicroUSD != 126 {
		t.Errorf("discarded work cost %d micro-USD, want 126", spent.CostMicroUSD)
	}
	if spent.UnmeteredAttempts != 0 {
		t.Errorf("unmetered attempts = %d, want 0: this attempt's usage was reported", spent.UnmeteredAttempts)
	}
	if !strings.Contains(spent.Error(), "126") {
		t.Errorf("error message hides the cost: %v", spent)
	}
}

// TestRetriedTimeoutsAreCountedAsUnmetered: a model that runs past the
// timeout has generated tokens the host will charge for, on every attempt.
// The amount cannot be known - no response arrived - but the caller must
// learn that the ledger is undercounting.
func TestRetriedTimeoutsAreCountedAsUnmetered(t *testing.T) {
	t.Parallel()

	host := newFakeHost(t, func(_ int, _ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	cfg := testConfig(host.URL)
	cfg.Timeout = 250 * time.Millisecond
	cfg.MaxAttempts = 2
	client, _ := newTestClient(t, cfg)

	_, err := client.Translate(t.Context(), sampleRequest)
	if !errors.Is(err, translation.ErrTimeout) {
		t.Fatalf("Translate() error = %v, want ErrTimeout", err)
	}

	var spent *translation.SpendError
	if !errors.As(err, &spent) {
		t.Fatalf("Translate() error = %v, want a *translation.SpendError flagging the billed attempts", err)
	}
	if spent.UnmeteredAttempts != 2 {
		t.Errorf("unmetered attempts = %d, want 2: both attempts were generating when they were abandoned", spent.UnmeteredAttempts)
	}
	if spent.CostMicroUSD != 0 {
		t.Errorf("cost = %d, want 0: no attempt ever reported usage, so no amount is known", spent.CostMicroUSD)
	}
	if spent.IsZero() {
		t.Error("Spend.IsZero() is true although two attempts were probably billed")
	}
}

// TestSuccessCarriesTheDiscardedAttempts: a translation that succeeded on
// the second attempt still cost whatever the first one did.
func TestSuccessCarriesTheDiscardedAttempts(t *testing.T) {
	t.Parallel()

	host := newFakeHost(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		if attempt == 1 {
			<-r.Context().Done()
			return
		}
		answerJSON(w, http.StatusOK, completion(translationJSON(wantHeadline, wantExtract), 200, 160))
	})
	cfg := testConfig(host.URL)
	cfg.Timeout = 250 * time.Millisecond
	client, _ := newTestClient(t, cfg)

	got, err := client.Translate(t.Context(), sampleRequest)
	if err != nil {
		t.Fatalf("Translate(): %v", err)
	}
	if got.CostMicroUSD != 126 {
		t.Errorf("cost = %d, want 126: the successful attempt's reported usage", got.CostMicroUSD)
	}
	if got.UnmeteredAttempts != 1 {
		t.Errorf("unmetered attempts = %d, want 1: the abandoned first attempt was probably billed too", got.UnmeteredAttempts)
	}
}

// TestFailureThatCostNothingIsAPlainError: a rejected key bought nothing,
// so a caller should not have to unwrap an error to discover that.
func TestFailureThatCostNothingIsAPlainError(t *testing.T) {
	t.Parallel()

	host := newFakeHost(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		answerJSON(w, http.StatusUnauthorized, `{"error":{"message":"Invalid API Key"}}`)
	})
	client, _ := newTestClient(t, testConfig(host.URL))

	_, err := client.Translate(t.Context(), sampleRequest)
	var spent *translation.SpendError
	if errors.As(err, &spent) {
		t.Errorf("Translate() error = %v, want a plain error: a refused request generates nothing to bill", err)
	}
}

// TestContextEndingDuringBackoffKeepsTheClassification: the reason the
// provider was being retried is the reason a pipeline decides whether to
// slow down, so it must survive a context that ends mid-wait.
func TestContextEndingDuringBackoffKeepsTheClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// endWait ends the caller's context the way this case wants,
		// standing in for a wait that outlives it.
		endWait func(cancel context.CancelFunc) error
		wantIs  []error
		wantNot []error
	}{
		{
			name:    "the caller's deadline passes",
			endWait: func(context.CancelFunc) error { return context.DeadlineExceeded },
			wantIs: []error{
				translation.ErrRateLimited,
				translation.ErrTimeout,
				context.DeadlineExceeded,
			},
		},
		{
			name:    "the caller gives up",
			endWait: func(context.CancelFunc) error { return context.Canceled },
			wantIs: []error{
				translation.ErrRateLimited,
				context.Canceled,
			},
			// Abandoning a run is not the provider being slow.
			wantNot: []error{translation.ErrTimeout},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusTooManyRequests, `{"error":{"message":"rate limit reached"}}`)
			})
			client, err := New(testConfig(host.URL))
			if err != nil {
				t.Fatalf("New(): %v", err)
			}
			// The wait is where the context ends, so the sleep reports
			// the ending rather than actually waiting for it.
			client.sleep = func(context.Context, time.Duration) error {
				return tc.endWait(func() {})
			}

			_, err = client.Translate(t.Context(), sampleRequest)
			if err == nil {
				t.Fatal("Translate() succeeded, want the interrupted retry reported")
			}
			for _, want := range tc.wantIs {
				if !errors.Is(err, want) {
					t.Errorf("errors.Is(err, %v) = false, want the chain to keep it: %v", want, err)
				}
			}
			for _, notWant := range tc.wantNot {
				if errors.Is(err, notWant) {
					t.Errorf("errors.Is(err, %v) = true, want it excluded: %v", notWant, err)
				}
			}
		})
	}
}

// TestRequestShape pins what actually goes on the wire: the endpoint, the
// bearer token, and a body carrying the configured model plus the released
// prompt's two turns.
func TestRequestShape(t *testing.T) {
	t.Parallel()

	host := newFakeHost(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		answerJSON(w, http.StatusOK, completion(translationJSON(wantHeadline, wantExtract), 200, 160))
	})
	client, _ := newTestClient(t, testConfig(host.URL))

	if _, err := client.Translate(t.Context(), sampleRequest); err != nil {
		t.Fatalf("Translate(): %v", err)
	}

	calls := host.recorded()
	if len(calls) != 1 {
		t.Fatalf("provider was called %d time(s), want 1", len(calls))
	}
	call := calls[0]

	if call.method != http.MethodPost {
		t.Errorf("method = %s, want POST", call.method)
	}
	if call.path != "/v1/chat/completions" {
		t.Errorf("path = %s, want /v1/chat/completions", call.path)
	}
	if call.authz != "Bearer test-key-value" {
		t.Errorf("Authorization = %q, want the configured key as a bearer token", call.authz)
	}
	if call.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", call.contentType)
	}
	if call.accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", call.accept)
	}

	var sent chatRequest
	if err := json.Unmarshal(call.body, &sent); err != nil {
		t.Fatalf("request body is not the chat-completions shape: %v (%s)", err, call.body)
	}
	if sent.Model != "configured-model" {
		t.Errorf("model = %q, want the configured model", sent.Model)
	}
	if sent.Temperature != 0 {
		t.Errorf("temperature = %v, want 0: translation is not a creative task", sent.Temperature)
	}
	if sent.MaxTokens != DefaultMaxOutputTokens {
		t.Errorf("max_tokens = %d, want the default output cap %d", sent.MaxTokens, DefaultMaxOutputTokens)
	}
	if sent.ResponseFormat.Type != "json_object" {
		t.Errorf("response_format = %q, want json_object", sent.ResponseFormat.Type)
	}
	if len(sent.Messages) != 2 {
		t.Fatalf("sent %d message(s), want a system turn and a user turn", len(sent.Messages))
	}
	if sent.Messages[0].Role != "system" || sent.Messages[1].Role != "user" {
		t.Errorf("message roles = %q, %q, want system then user", sent.Messages[0].Role, sent.Messages[1].Role)
	}

	prompt, err := translation.PromptByVersion(sampleRequest.PromptVersion)
	if err != nil {
		t.Fatalf("PromptByVersion(): %v", err)
	}
	if sent.Messages[0].Content != prompt.System {
		t.Errorf("system turn is not the released prompt %q verbatim:\n%s", prompt.Version, sent.Messages[0].Content)
	}
	if sent.Messages[1].Content != prompt.UserMessage(sampleRequest) {
		t.Errorf("user turn is not the rendered prompt template:\n%s", sent.Messages[1].Content)
	}
	for _, want := range []string{sampleRequest.SourceTitle, sampleRequest.SourceText, "el", "de"} {
		if !strings.Contains(sent.Messages[1].Content, want) {
			t.Errorf("user turn is missing %q", want)
		}
	}
}

// TestNoAPIKeyOmitsTheHeader: a self-hosted server started without a key
// must not be sent an empty bearer token.
func TestNoAPIKeyOmitsTheHeader(t *testing.T) {
	t.Parallel()

	host := newFakeHost(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		answerJSON(w, http.StatusOK, completion(translationJSON(wantHeadline, wantExtract), 10, 10))
	})
	cfg := testConfig(host.URL)
	cfg.APIKey = ""
	client, _ := newTestClient(t, cfg)

	if _, err := client.Translate(t.Context(), sampleRequest); err != nil {
		t.Fatalf("Translate(): %v", err)
	}
	if authz := host.recorded()[0].authz; authz != "" {
		t.Errorf("Authorization = %q, want no header at all", authz)
	}
}

// TestBackoffGrows checks the waits themselves, not just that retries
// happened: the second wait must exceed the first.
func TestBackoffGrows(t *testing.T) {
	t.Parallel()

	host := newFakeHost(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		answerJSON(w, http.StatusInternalServerError, `{"error":"internal"}`)
	})
	cfg := testConfig(host.URL)
	cfg.MaxAttempts = 4
	client, waits := newTestClient(t, cfg)

	if _, err := client.Translate(t.Context(), sampleRequest); !errors.Is(err, translation.ErrUnavailable) {
		t.Fatalf("Translate() error = %v, want ErrUnavailable", err)
	}

	got := waits()
	if len(got) != 3 {
		t.Fatalf("backed off %d time(s), want 3", len(got))
	}
	if got[0] != cfg.BaseBackoff {
		t.Errorf("first wait = %s, want the configured base %s", got[0], cfg.BaseBackoff)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Errorf("wait %d (%s) does not exceed wait %d (%s): backoff must grow", i, got[i], i-1, got[i-1])
		}
	}
}

// TestRetryAfterIsHonoured: a host that says how long to wait is believed,
// up to the adapter's cap.
func TestRetryAfterIsHonoured(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		retryAfter string
		want       time.Duration
	}{
		{name: "seconds", retryAfter: "2", want: 2 * time.Second},
		{name: "fractional seconds", retryAfter: "0.5", want: 500 * time.Millisecond},
		{name: "absurd wait is capped", retryAfter: "3600", want: maxBackoff},
		{name: "http date form falls back to our backoff", retryAfter: "Wed, 21 Oct 2026 07:28:00 GMT", want: 10 * time.Millisecond},
		{name: "nonsense falls back to our backoff", retryAfter: "soon", want: 10 * time.Millisecond},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost(t, func(attempt int, w http.ResponseWriter, _ *http.Request) {
				if attempt == 1 {
					w.Header().Set("Retry-After", tc.retryAfter)
					answerJSON(w, http.StatusTooManyRequests, `{"error":"slow down"}`)
					return
				}
				answerJSON(w, http.StatusOK, completion(translationJSON(wantHeadline, wantExtract), 100, 100))
			})
			client, waits := newTestClient(t, testConfig(host.URL))

			if _, err := client.Translate(t.Context(), sampleRequest); err != nil {
				t.Fatalf("Translate(): %v", err)
			}
			got := waits()
			if len(got) != 1 {
				t.Fatalf("backed off %d time(s), want 1", len(got))
			}
			if got[0] != tc.want {
				t.Errorf("waited %s, want %s", got[0], tc.want)
			}
		})
	}
}

// TestTimeout: a host that never answers costs one bounded wait per
// attempt and is reported as a timeout, not as an outage.
func TestTimeout(t *testing.T) {
	t.Parallel()

	host := newFakeHost(t, func(_ int, _ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	cfg := testConfig(host.URL)
	cfg.Timeout = 250 * time.Millisecond
	cfg.MaxAttempts = 2
	client, waits := newTestClient(t, cfg)

	start := time.Now()
	_, err := client.Translate(t.Context(), sampleRequest)
	elapsed := time.Since(start)

	if !errors.Is(err, translation.ErrTimeout) {
		t.Fatalf("Translate() error = %v, want ErrTimeout", err)
	}
	if errors.Is(err, translation.ErrUnavailable) {
		t.Error("a timeout must not also read as an outage: the two lead to different decisions")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Translate() took %s; the per-attempt timeout did not bound it", elapsed)
	}
	if calls := len(host.recorded()); calls != 2 {
		t.Errorf("provider was called %d time(s), want 2: a timeout is retryable", calls)
	}
	if n := len(waits()); n != 1 {
		t.Errorf("backed off %d time(s), want 1", n)
	}
}

// TestCallerDeadline: a deadline the caller set is that caller's, and it
// stops the adapter retrying against it.
func TestCallerDeadline(t *testing.T) {
	t.Parallel()

	host := newFakeHost(t, func(_ int, _ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	client, _ := newTestClient(t, testConfig(host.URL))

	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()

	_, err := client.Translate(ctx, sampleRequest)
	if !errors.Is(err, translation.ErrTimeout) {
		t.Fatalf("Translate() error = %v, want ErrTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Translate() error = %v, want the caller's deadline preserved in the chain", err)
	}
	// At most one attempt: retrying against a deadline that has already
	// passed can only burn the caller's remaining time.
	if calls := len(host.recorded()); calls > 1 {
		t.Errorf("provider was called %d times: a spent caller deadline is not retried against", calls)
	}
}

// TestCallerCancellation: an abandoned run reports cancellation, never a
// provider fault.
func TestCallerCancellation(t *testing.T) {
	t.Parallel()

	host := newFakeHost(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		answerJSON(w, http.StatusOK, completion(translationJSON(wantHeadline, wantExtract), 10, 10))
	})
	client, _ := newTestClient(t, testConfig(host.URL))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := client.Translate(ctx, sampleRequest)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Translate() error = %v, want context.Canceled", err)
	}
	if errors.Is(err, translation.ErrUnavailable) || errors.Is(err, translation.ErrTimeout) {
		t.Errorf("a cancelled run must not be blamed on the provider: %v", err)
	}
}

// TestUnreachableHost: nothing listening is an outage, retried and then
// reported as one.
func TestUnreachableHost(t *testing.T) {
	t.Parallel()

	host := newFakeHost(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		answerJSON(w, http.StatusOK, "{}")
	})
	cfg := testConfig(host.URL)
	host.Close()

	cfg.MaxAttempts = 2
	client, waits := newTestClient(t, cfg)

	_, err := client.Translate(t.Context(), sampleRequest)
	if !errors.Is(err, translation.ErrUnavailable) {
		t.Fatalf("Translate() error = %v, want ErrUnavailable", err)
	}
	if n := len(waits()); n != 1 {
		t.Errorf("backed off %d time(s), want 1", n)
	}
}

// TestTranslateRejectsUnusableRequests: money is only spent on a request
// that could produce a recordable row.
func TestTranslateRejectsUnusableRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  translation.Request
	}{
		{
			name: "blank source title",
			req: translation.Request{
				SourceText:     "text",
				SourceLanguage: "el",
				TargetLanguage: "de",
				PromptVersion:  translation.CurrentPromptVersion,
			},
		},
		{
			name: "unreleased prompt version",
			req: translation.Request{
				SourceTitle:    "title",
				SourceText:     "text",
				SourceLanguage: "el",
				TargetLanguage: "de",
				PromptVersion:  "v99",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			host := newFakeHost(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, http.StatusOK, completion(translationJSON(wantHeadline, wantExtract), 10, 10))
			})
			client, _ := newTestClient(t, testConfig(host.URL))

			_, err := client.Translate(t.Context(), tc.req)
			if !errors.Is(err, translation.ErrInvalidRequest) {
				t.Fatalf("Translate() error = %v, want ErrInvalidRequest", err)
			}
			if calls := len(host.recorded()); calls != 0 {
				t.Errorf("provider was called %d time(s), want 0: an unusable request never reaches the wire", calls)
			}
		})
	}
}

func TestCostMicroUSD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		usage       usage
		inputPrice  float64
		outputPrice float64
		want        int64
		wantErr     bool
	}{
		{
			name:        "groq gpt-oss-120b, one news item",
			usage:       usage{PromptTokens: 210, CompletionTokens: 160},
			inputPrice:  0.15,
			outputPrice: 0.60,
			// 210 * 0.15 + 160 * 0.60 = 31.5 + 96 = 127.5 -> 128.
			want: 128,
		},
		{
			name:        "deepinfra gpt-oss-120b, one news item",
			usage:       usage{PromptTokens: 210, CompletionTokens: 160},
			inputPrice:  0.037,
			outputPrice: 0.17,
			// 210 * 0.037 + 160 * 0.17 = 7.77 + 27.2 = 34.97 -> 35.
			want: 35,
		},
		{
			name:        "a whole alpha month of tokens stays well inside the cap",
			usage:       usage{PromptTokens: 630_000, CompletionTokens: 504_000},
			inputPrice:  0.15,
			outputPrice: 0.60,
			// 630000 * 0.15 + 504000 * 0.60 = 94500 + 302400 = 396900
			// micro-USD, i.e. $0.397 against the $25 monthly cap.
			want: 396_900,
		},
		{
			name:        "self-hosted server costs nothing per token",
			usage:       usage{PromptTokens: 5000, CompletionTokens: 4000},
			inputPrice:  0,
			outputPrice: 0,
			want:        0,
		},
		{
			name:        "sub-micro-USD work rounds to zero",
			usage:       usage{PromptTokens: 1, CompletionTokens: 1},
			inputPrice:  0.037,
			outputPrice: 0.17,
			// 0.207 micro-USD: below the unit the column stores.
			want: 0,
		},
		{
			name:        "rounding is to the nearest micro-USD",
			usage:       usage{PromptTokens: 10, CompletionTokens: 0},
			inputPrice:  0.15,
			outputPrice: 0.60,
			// 1.5 -> 2 (half away from zero).
			want: 2,
		},
		{
			name:        "no output tokens costs input only",
			usage:       usage{PromptTokens: 400, CompletionTokens: 0},
			inputPrice:  0.25,
			outputPrice: 1.10,
			want:        100,
		},
		{
			name:        "negative usage is refused",
			usage:       usage{PromptTokens: -1, CompletionTokens: 10},
			inputPrice:  0.15,
			outputPrice: 0.60,
			wantErr:     true,
		},
		{
			name:        "negative completion usage is refused",
			usage:       usage{PromptTokens: 10, CompletionTokens: -1},
			inputPrice:  0.15,
			outputPrice: 0.60,
			wantErr:     true,
		},
		{
			name:        "a cost too large to represent is refused",
			usage:       usage{PromptTokens: 9_000_000_000_000_000_000, CompletionTokens: 0},
			inputPrice:  1000,
			outputPrice: 1000,
			wantErr:     true,
		},
		{
			// 2^62 tokens at 2 micro-USD each is exactly 2^63, one above
			// the largest int64. float64(math.MaxInt64) rounds up to the
			// same 2^63, so a guard written against MaxInt64 waves this
			// through and the conversion wraps it negative - which would
			// then fail the column's non-negative check, or worse, credit
			// the ledger.
			name:        "a cost of exactly 2^63 is refused",
			usage:       usage{PromptTokens: 1 << 62, CompletionTokens: 0},
			inputPrice:  2,
			outputPrice: 0,
			wantErr:     true,
		},
		{
			// Half the boundary still has to work: the guard must reject
			// what overflows and nothing else. (The nearest representable
			// value to the boundary itself is the boundary, so the
			// largest accepted cost cannot be expressed exactly in
			// float64 - which is why the guard is an inequality against
			// 2^63 rather than an attempt to name the last good value.)
			name:        "a very large but representable cost is accepted",
			usage:       usage{PromptTokens: 1 << 61, CompletionTokens: 0},
			inputPrice:  2,
			outputPrice: 0,
			want:        1 << 62,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := costMicroUSD(tc.usage, tc.inputPrice, tc.outputPrice)
			if tc.wantErr {
				if !errors.Is(err, translation.ErrInvalidResponse) {
					t.Fatalf("costMicroUSD() error = %v, want ErrInvalidResponse", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("costMicroUSD() error = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("costMicroUSD() = %d, want %d micro-USD", got, tc.want)
			}
		})
	}
}

func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()

	valid := Config{
		BaseURL:                  "https://api.groq.com/openai/v1",
		Model:                    "a-model",
		InputPricePerMillionUSD:  0.15,
		OutputPricePerMillionUSD: 0.60,
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "complete config", mutate: func(*Config) {}},
		{name: "missing base url", mutate: func(c *Config) { c.BaseURL = "  " }, wantErr: "base URL is required"},
		{name: "base url without a scheme", mutate: func(c *Config) { c.BaseURL = "api.groq.com/openai/v1" }, wantErr: "must be http or https"},
		{name: "base url with the wrong scheme", mutate: func(c *Config) { c.BaseURL = "ftp://api.groq.com/v1" }, wantErr: "must be http or https"},
		{name: "base url without a host", mutate: func(c *Config) { c.BaseURL = "http:///v1" }, wantErr: "has no host"},
		{name: "unparsable base url", mutate: func(c *Config) { c.BaseURL = "http://a b.example/v1" }, wantErr: "not a URL"},
		{name: "missing model", mutate: func(c *Config) { c.Model = "" }, wantErr: "model is required"},
		{name: "negative input price", mutate: func(c *Config) { c.InputPricePerMillionUSD = -0.1 }, wantErr: "input price"},
		{name: "negative output price", mutate: func(c *Config) { c.OutputPricePerMillionUSD = -0.1 }, wantErr: "output price"},
		// A forgotten price line must never look like a free host.
		{name: "unset input price", mutate: func(c *Config) { c.InputPricePerMillionUSD = 0 }, wantErr: "set FreeOfCharge"},
		{name: "unset output price", mutate: func(c *Config) { c.OutputPricePerMillionUSD = 0 }, wantErr: "set FreeOfCharge"},
		{name: "unset prices entirely", mutate: func(c *Config) {
			c.InputPricePerMillionUSD, c.OutputPricePerMillionUSD = 0, 0
		}, wantErr: "set FreeOfCharge"},
		{name: "free host contradicted by a price", mutate: func(c *Config) { c.FreeOfCharge = true }, wantErr: "free or it is priced"},
		{name: "free host with no prices", mutate: func(c *Config) {
			c.FreeOfCharge = true
			c.InputPricePerMillionUSD, c.OutputPricePerMillionUSD = 0, 0
		}},
		{name: "negative timeout", mutate: func(c *Config) { c.Timeout = -time.Second }, wantErr: "must not be negative"},
		{name: "negative attempts", mutate: func(c *Config) { c.MaxAttempts = -1 }, wantErr: "must not be negative"},
		{name: "negative backoff", mutate: func(c *Config) { c.BaseBackoff = -time.Second }, wantErr: "must not be negative"},
		{name: "negative output cap", mutate: func(c *Config) { c.MaxOutputTokens = -1 }, wantErr: "must not be negative"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := valid
			tc.mutate(&cfg)
			client, err := New(cfg)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("New() = %v, want a client", err)
				}
				if client.endpoint != "https://api.groq.com/openai/v1/chat/completions" {
					t.Errorf("endpoint = %q, want the base URL plus /chat/completions", client.endpoint)
				}
				return
			}
			if err == nil {
				t.Fatalf("New() succeeded, want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("New() = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestNewRejectsUnpriceableConfig(t *testing.T) {
	t.Parallel()

	for _, price := range []float64{math.NaN(), math.Inf(1)} {
		input := Config{
			BaseURL:                  "https://api.together.ai/v1",
			Model:                    "a-model",
			InputPricePerMillionUSD:  price,
			OutputPricePerMillionUSD: 0.25,
		}
		if _, err := New(input); err == nil || !strings.Contains(err.Error(), "input price") {
			t.Errorf("New() error for the input price %v = %v, want it refused as unpriceable", price, err)
		}

		output := Config{
			BaseURL:                  "https://api.together.ai/v1",
			Model:                    "a-model",
			InputPricePerMillionUSD:  0.17,
			OutputPricePerMillionUSD: price,
		}
		if _, err := New(output); err == nil || !strings.Contains(err.Error(), "output price") {
			t.Errorf("New() error for the output price %v = %v, want it refused as unpriceable", price, err)
		}
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	t.Parallel()

	client, err := New(Config{
		BaseURL:      "http://vllm.internal:8000/v1/",
		Model:        "local-model",
		FreeOfCharge: true,
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	if client.cfg.Timeout != DefaultTimeout {
		t.Errorf("timeout = %s, want %s", client.cfg.Timeout, DefaultTimeout)
	}
	if client.cfg.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("attempts = %d, want %d", client.cfg.MaxAttempts, DefaultMaxAttempts)
	}
	if client.cfg.BaseBackoff != DefaultBaseBackoff {
		t.Errorf("backoff = %s, want %s", client.cfg.BaseBackoff, DefaultBaseBackoff)
	}
	if client.cfg.MaxOutputTokens != DefaultMaxOutputTokens {
		t.Errorf("output cap = %d, want %d", client.cfg.MaxOutputTokens, DefaultMaxOutputTokens)
	}
	if client.http == nil {
		t.Error("client has no HTTP client")
	}
	// A trailing slash on the base URL must not double up in the path.
	if client.endpoint != "http://vllm.internal:8000/v1/chat/completions" {
		t.Errorf("endpoint = %q, want the trailing slash collapsed", client.endpoint)
	}
}

func TestNewUsesTheSuppliedHTTPClient(t *testing.T) {
	t.Parallel()

	supplied := &http.Client{Timeout: time.Minute}
	client, err := New(Config{
		BaseURL:                  "https://api.deepinfra.com/v1/openai",
		Model:                    "a-model",
		InputPricePerMillionUSD:  0.037,
		OutputPricePerMillionUSD: 0.17,
		HTTPClient:               supplied,
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if client.http != supplied {
		t.Error("New() replaced the supplied HTTP client")
	}
}

func TestJSONObjects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "no object", in: "just prose", want: nil},
		{name: "one object", in: `{"a":1}`, want: []string{`{"a":1}`}},
		{name: "object in a fence", in: "```json\n{\"a\":1}\n```", want: []string{`{"a":1}`}},
		{name: "two objects", in: `{"a":1} and {"b":2}`, want: []string{`{"a":1}`, `{"b":2}`}},
		{name: "nested object counts once", in: `{"a":{"b":1}}`, want: []string{`{"a":{"b":1}}`}},
		{name: "braces inside a string are text", in: `{"a":"}{"}`, want: []string{`{"a":"}{"}`}},
		{name: "escaped quote does not end the string", in: `{"a":"say \"}\" here"}`, want: []string{`{"a":"say \"}\" here"}`}},
		{name: "stray closing brace is ignored", in: `} {"a":1}`, want: []string{`{"a":1}`}},
		{name: "unterminated object yields nothing", in: `{"a":1`, want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := jsonObjects(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("jsonObjects(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("jsonObjects(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSnippetOf(t *testing.T) {
	t.Parallel()

	if got := snippetOf(nil); got != "(empty body)" {
		t.Errorf("snippetOf(nil) = %q, want a marker for an empty body", got)
	}
	if got := snippetOf([]byte("  short  ")); got != "short" {
		t.Errorf("snippetOf() = %q, want the trimmed text", got)
	}

	long := strings.Repeat("α", errorBodySnippet*2)
	got := snippetOf([]byte(long))
	if !strings.HasSuffix(got, "...") {
		t.Errorf("snippetOf() = %q, want a truncation marker", got)
	}
	if len([]rune(got)) != errorBodySnippet+3 {
		t.Errorf("snippetOf() kept %d runes, want %d plus the marker", len([]rune(got)), errorBodySnippet)
	}
	// Truncation must not split a multi-byte character.
	if strings.Contains(got, "�") {
		t.Error("snippetOf() split a multi-byte character")
	}
}

func TestSleepContext(t *testing.T) {
	t.Parallel()

	if err := sleepContext(t.Context(), 0); err != nil {
		t.Errorf("sleepContext(0) = %v, want nil", err)
	}
	if err := sleepContext(t.Context(), time.Millisecond); err != nil {
		t.Errorf("sleepContext(1ms) = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleepContext() on a cancelled context = %v, want context.Canceled", err)
	}
}

func TestBackoffIsBounded(t *testing.T) {
	t.Parallel()

	if got := backoff(20, time.Second, 0); got != maxBackoff {
		t.Errorf("backoff(20) = %s, want it capped at %s", got, maxBackoff)
	}
	if got := backoff(1, time.Second, 0); got != time.Second {
		t.Errorf("backoff(1) = %s, want the base %s", got, time.Second)
	}
	if got := backoff(3, time.Second, 0); got != 4*time.Second {
		t.Errorf("backoff(3) = %s, want 4s", got)
	}
}

// TestAdapterWorksThroughTheModuleInterface states the point of the
// package: a consumer holds a translation.Translator and never learns
// which host answered it.
func TestAdapterWorksThroughTheModuleInterface(t *testing.T) {
	t.Parallel()

	host := newFakeHost(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		answerJSON(w, http.StatusOK, completion(translationJSON(wantHeadline, wantExtract), 200, 160))
	})
	client, _ := newTestClient(t, testConfig(host.URL))

	var translator translation.Translator = client
	got, err := translator.Translate(t.Context(), sampleRequest)
	if err != nil {
		t.Fatalf("Translate() through the interface: %v", err)
	}
	if got.Headline != wantHeadline || got.Extract != wantExtract {
		t.Errorf("Translate() = %+v, want the translated headline and extract", got)
	}
	if got.CostMicroUSD == 0 {
		t.Error("a priced translation must carry its cost: translation.cost_microusd has no default (FR-006)")
	}
}

func TestErrorsQuoteTheProviderBody(t *testing.T) {
	t.Parallel()

	host := newFakeHost(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		answerJSON(w, http.StatusUnauthorized, `{"error":{"message":"Invalid API Key"}}`)
	})
	client, _ := newTestClient(t, testConfig(host.URL))

	_, err := client.Translate(t.Context(), sampleRequest)
	if err == nil {
		t.Fatal("Translate() succeeded, want an auth failure")
	}
	if !strings.Contains(err.Error(), "Invalid API Key") {
		t.Errorf("error does not carry the provider's explanation: %v", err)
	}
	if strings.Contains(err.Error(), "test-key-value") {
		t.Errorf("error leaks the API key: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(http.StatusUnauthorized)) {
		t.Errorf("error does not name the status: %v", err)
	}
}
