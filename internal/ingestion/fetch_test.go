package ingestion_test

// Retrieval is tested against local test servers - and, where the point is
// what we do NOT read, against a stub transport. Nothing here reaches the
// network, and no test waits out a real backoff: the waits that matter are
// asserted by making the alternative take thirty seconds.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/ingestion"
)

// feedFixture is the document the fetcher tests serve: a two-entry RSS 2.0
// feed, Greek and German, from the same embedded testdata the parser tests
// read.
func feedFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := fixtures.ReadFile("testdata/fetch_feed.xml")
	if err != nil {
		t.Fatalf("reading the feed fixture: %v", err)
	}
	return raw
}

// stubTransport answers every request with the same prepared response, so a
// test can state exactly what arrived - headers a real server would not let
// it declare included.
type stubTransport struct{ resp *http.Response }

func (s stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return s.resp, nil
}

// watchedBody records whether anything ever read it.
type watchedBody struct {
	r    io.Reader
	read atomic.Bool
}

func (b *watchedBody) Read(p []byte) (int, error) {
	b.read.Store(true)
	return b.r.Read(p)
}

func (b *watchedBody) Close() error { return nil }

func TestFetchParsesAFixtureFeed(t *testing.T) {
	t.Parallel()

	feed := feedFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Fri, 08 Aug 2025 07:00:00 GMT")
		_, _ = w.Write(feed)
	}))
	defer server.Close()

	result, err := ingestion.FetchConfig{}.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if result.NotModified {
		t.Error("a 200 was reported as not modified")
	}
	if got, want := len(result.Items), 2; got != want {
		t.Fatalf("got %d items, want %d", got, want)
	}
	first := result.Items[0]
	if got, want := first.Title, "Συνέλευση της κοινότητας τον Σεπτέμβριο"; got != want {
		t.Errorf("first title = %q, want %q", got, want)
	}
	if got, want := first.Link, "https://feed.example.test/articles/synelefsi"; got != want {
		t.Errorf("first link = %q, want %q", got, want)
	}
	if got, want := first.Author, "Γιώργος Αντωνίου"; got != want {
		t.Errorf("first author = %q, want %q", got, want)
	}
	if !equalTime(first.Published, ts(t, "2025-08-07T18:00:00+03:00")) {
		t.Errorf("first published = %v, want 2025-08-07T18:00:00+03:00", first.Published)
	}
	if got, want := result.Items[1].Title, "Griechischer Sprachkurs startet im Herbst"; got != want {
		t.Errorf("second title = %q, want %q", got, want)
	}
	// The validators the next poll will send back.
	if got, want := result.ETag, `"v1"`; got != want {
		t.Errorf("ETag = %q, want %q", got, want)
	}
	if got, want := result.LastModified, "Fri, 08 Aug 2025 07:00:00 GMT"; got != want {
		t.Errorf("Last-Modified = %q, want %q", got, want)
	}
}

func TestFetchSendsTheStoredValidators(t *testing.T) {
	t.Parallel()

	feed := feedFixture(t)
	tests := []struct {
		name              string
		stored            ingestion.Validators
		wantNoneMatch     string
		wantModifiedSince string
	}{
		{
			name:              "both stored",
			stored:            ingestion.Validators{ETag: `"v1"`, LastModified: "Fri, 08 Aug 2025 07:00:00 GMT"},
			wantNoneMatch:     `"v1"`,
			wantModifiedSince: "Fri, 08 Aug 2025 07:00:00 GMT",
		},
		{
			// A first-ever poll has nothing to be conditional about, and
			// sending an empty validator would ask the source to match "".
			name:   "nothing stored yet",
			stored: ingestion.Validators{},
		},
		{
			name:          "only an etag",
			stored:        ingestion.Validators{ETag: `"v2"`},
			wantNoneMatch: `"v2"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotNoneMatch, gotModifiedSince string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotNoneMatch = r.Header.Get("If-None-Match")
				gotModifiedSince = r.Header.Get("If-Modified-Since")
				_, _ = w.Write(feed)
			}))
			defer server.Close()

			if _, err := (ingestion.FetchConfig{}).Fetch(t.Context(), server.URL+"/feed.xml", tt.stored); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if gotNoneMatch != tt.wantNoneMatch {
				t.Errorf("If-None-Match = %q, want %q", gotNoneMatch, tt.wantNoneMatch)
			}
			if gotModifiedSince != tt.wantModifiedSince {
				t.Errorf("If-Modified-Since = %q, want %q", gotModifiedSince, tt.wantModifiedSince)
			}
		})
	}
}

func TestFetchNotModifiedYieldsNoItems(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	result, err := ingestion.FetchConfig{}.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{ETag: `"v1"`})
	// A 304 is the outcome conditional GET exists to produce, not a
	// failure: the poll loop records that it asked and moves on.
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !result.NotModified {
		t.Error("a 304 was not reported as not modified")
	}
	if len(result.Items) != 0 {
		t.Errorf("got %d items from a 304, want none", len(result.Items))
	}
	if got, want := result.ETag, `"v1"`; got != want {
		t.Errorf("ETag = %q, want %q", got, want)
	}
}

func TestFetchHonoursRetryAfterButClampsIt(t *testing.T) {
	t.Parallel()

	t.Run("a wait longer than the ceiling is clamped", func(t *testing.T) {
		t.Parallel()

		var hits atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer server.Close()

		// One attempt, so nothing is waited out here: what is asserted is
		// the wait the poll loop is handed, not a sleep.
		cfg := ingestion.FetchConfig{MaxAttempts: 1}
		result, err := cfg.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
		if !errors.Is(err, ingestion.ErrRateLimited) {
			t.Fatalf("error = %v, want ErrRateLimited", err)
		}
		if got, want := result.RetryAfter, 30*time.Second; got != want {
			t.Errorf("RetryAfter = %v, want %v (the ten minutes asked for, clamped)", got, want)
		}
		if got := hits.Load(); got != 1 {
			t.Errorf("source was asked %d times, want 1", got)
		}
	})

	t.Run("an overflowing or unreadable wait is the ceiling, never negative", func(t *testing.T) {
		t.Parallel()

		// Values whose float-to-int conversion would overflow int64 - an
		// epoch-milliseconds timestamp, an absurd exponent, a NaN. Clamped
		// after converting, each lands on math.MinInt64: a negative "wait"
		// the poll loop would read as "poll again immediately", the exact
		// opposite of what the source asked. A genuine number over the
		// ceiling is the ceiling; NaN is not an instruction at all and
		// falls back to our own backoff.
		cases := map[string]time.Duration{
			"99999999999": 30 * time.Second,
			"1e300":       30 * time.Second,
			"NaN":         0,
		}
		for value, want := range cases {
			t.Run(value, func(t *testing.T) {
				t.Parallel()

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Retry-After", value)
					w.WriteHeader(http.StatusTooManyRequests)
				}))
				defer server.Close()

				cfg := ingestion.FetchConfig{MaxAttempts: 1}
				result, err := cfg.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
				if !errors.Is(err, ingestion.ErrRateLimited) {
					t.Fatalf("error = %v, want ErrRateLimited", err)
				}
				if result.RetryAfter < 0 {
					t.Fatalf("RetryAfter = %v: negative, the clamp overflowed", result.RetryAfter)
				}
				if result.RetryAfter != want {
					t.Errorf("RetryAfter = %v, want %v", result.RetryAfter, want)
				}
			})
		}
	})

	t.Run("a wait over the ceiling ends the call's retrying", func(t *testing.T) {
		t.Parallel()

		var hits atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer server.Close()

		// Three attempts are allowed. Re-asking under a 30s cap a source
		// that said "ten minutes" would be hammering it against its own
		// instruction, so the call must end on the first answer.
		cfg := ingestion.FetchConfig{MaxAttempts: 3, BaseBackoff: 30 * time.Second}
		started := time.Now()
		_, err := cfg.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
		if !errors.Is(err, ingestion.ErrRateLimited) {
			t.Fatalf("error = %v, want ErrRateLimited", err)
		}
		if got := hits.Load(); got != 1 {
			t.Errorf("source was asked %d times, want 1: it asked to be left alone for longer than we ever wait", got)
		}
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Errorf("the over-ceiling ask took %v, so it was waited out and retried", elapsed)
		}
	})

	t.Run("a short wait is honoured over our own backoff", func(t *testing.T) {
		t.Parallel()

		feed := feedFixture(t)
		var hits atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if hits.Add(1) == 1 {
				w.Header().Set("Retry-After", "0.05")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = w.Write(feed)
		}))
		defer server.Close()

		// The base backoff is the ceiling itself, so finishing quickly is
		// only possible if the source's fifty milliseconds were used.
		cfg := ingestion.FetchConfig{MaxAttempts: 2, BaseBackoff: 30 * time.Second}
		started := time.Now()
		result, err := cfg.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if got, want := len(result.Items), 2; got != want {
			t.Errorf("got %d items, want %d", got, want)
		}
		if got := hits.Load(); got != 2 {
			t.Errorf("source was asked %d times, want 2", got)
		}
		if elapsed > 10*time.Second {
			t.Errorf("the retry took %v, so the base backoff was used instead of the source's Retry-After", elapsed)
		}
	})
}

func TestFetchRefusesAnOversizedBody(t *testing.T) {
	t.Parallel()

	t.Run("a declared length over the limit", func(t *testing.T) {
		t.Parallel()

		// The body is a perfectly good feed; only the declared length is
		// oversized. Reading it would therefore succeed, which is what
		// makes the untouched body proof that the parser never ran.
		body := &watchedBody{r: bytes.NewReader(feedFixture(t))}
		cfg := ingestion.FetchConfig{
			Client: &http.Client{Transport: stubTransport{resp: &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{},
				ContentLength: int64(ingestion.MaxFeedBytes) + 1,
				Body:          body,
			}}},
		}

		result, err := cfg.Fetch(t.Context(), "https://feed.example.test/feed.xml", ingestion.Validators{})
		if !errors.Is(err, ingestion.ErrFeedTooLarge) {
			t.Fatalf("error = %v, want ErrFeedTooLarge", err)
		}
		if len(result.Items) != 0 {
			t.Errorf("got %d items from a refused document, want none", len(result.Items))
		}
		if body.read.Load() {
			t.Error("the oversized body was read; a document whose declared length is over the limit must be refused before the parser sees it")
		}
	})

	t.Run("a body that keeps arriving past the limit", func(t *testing.T) {
		t.Parallel()

		// A declared length is only a claim, and a chunked response makes
		// no claim at all, so the guarantee is the bound the parser reads
		// through. A valid feed padded past it with comment bytes, streamed
		// so neither side holds an oversized document in memory.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.Copy(w, io.MultiReader(
				strings.NewReader(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title><!--`),
				io.LimitReader(padding{}, ingestion.MaxFeedBytes),
				strings.NewReader(`--><item><title>i</title><link>https://example.test/i</link></item></channel></rss>`),
			))
		}))
		defer server.Close()

		result, err := ingestion.FetchConfig{}.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
		if !errors.Is(err, ingestion.ErrFeedTooLarge) {
			t.Fatalf("error = %v, want ErrFeedTooLarge", err)
		}
		if len(result.Items) != 0 {
			t.Errorf("got %d items from a refused document, want none", len(result.Items))
		}
	})
}

func TestFetchRefusesANonHTTPRedirect(t *testing.T) {
	t.Parallel()

	t.Run("a hop to another scheme", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Following this would turn a feed poll into a local file read.
			http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
		}))
		defer server.Close()

		_, err := ingestion.FetchConfig{}.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
		if !errors.Is(err, ingestion.ErrUnsafeLocation) {
			t.Fatalf("error = %v, want ErrUnsafeLocation", err)
		}
	})

	t.Run("a configured location that is not http", func(t *testing.T) {
		t.Parallel()

		_, err := ingestion.FetchConfig{}.Fetch(t.Context(), "file:///etc/passwd", ingestion.Validators{})
		if !errors.Is(err, ingestion.ErrUnsafeLocation) {
			t.Fatalf("error = %v, want ErrUnsafeLocation", err)
		}
	})
}

func TestFetchIdentifiesItselfOnEveryRequest(t *testing.T) {
	t.Parallel()

	feed := feedFixture(t)
	tests := []struct {
		name      string
		userAgent string
		want      string
	}{
		{
			name:      "the operator's own identification",
			userAgent: "Apivo News (operator@example.test)",
			want:      "Apivo News (operator@example.test)",
		},
		{
			// Never anonymous, never a browser's: a source that wants us
			// gone must have somebody to say so to.
			name: "the default when the operator states none",
			want: ingestion.DefaultUserAgent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Two servers, so two handler goroutines write here.
			var (
				mu   sync.Mutex
				seen []string
			)
			record := func(r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				seen = append(seen, r.Header.Get("User-Agent"))
			}

			final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				record(r)
				_, _ = w.Write(feed)
			}))
			defer final.Close()

			moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				record(r)
				http.Redirect(w, r, final.URL+"/feed.xml", http.StatusMovedPermanently)
			}))
			defer moved.Close()

			cfg := ingestion.FetchConfig{UserAgent: tt.userAgent}
			if _, err := cfg.Fetch(t.Context(), moved.URL+"/feed.xml", ingestion.Validators{}); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 2 {
				t.Fatalf("saw %d requests, want 2 (the original and the hop)", len(seen))
			}
			for i, agent := range seen {
				if agent != tt.want {
					t.Errorf("request %d identified itself as %q, want %q", i+1, agent, tt.want)
				}
			}
		})
	}
}

func TestFetchDistinguishesCallerCancellationFromTheAttemptDeadline(t *testing.T) {
	t.Parallel()

	// The two are told apart wherever they can happen - before the answer
	// arrives, part-way through the document, and while we are waiting to
	// retry - because the poll loop treats them differently: a cancelled
	// poll is a shutdown, a timed-out one is a source that needs watching.
	hang := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	// stall sends the opening of a feed and then stops, so the failure
	// lands in the middle of the parser's read rather than before it.
	stall := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	t.Run("the caller gave up", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(hang)
		defer server.Close()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(50*time.Millisecond, cancel)

		cfg := ingestion.FetchConfig{MaxAttempts: 1, Timeout: time.Minute}
		_, err := cfg.Fetch(ctx, server.URL+"/feed.xml", ingestion.Validators{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if errors.Is(err, ingestion.ErrTimeout) {
			t.Errorf("a cancelled poll was reported as a timeout: %v", err)
		}
	})

	t.Run("the attempt ran out of time", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(hang)
		defer server.Close()

		cfg := ingestion.FetchConfig{MaxAttempts: 1, Timeout: 50 * time.Millisecond}
		_, err := cfg.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
		if !errors.Is(err, ingestion.ErrTimeout) {
			t.Fatalf("error = %v, want ErrTimeout", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Errorf("our own deadline was reported as the caller's cancellation: %v", err)
		}
		if !strings.Contains(err.Error(), "50ms") {
			t.Errorf("the error does not name the deadline that passed: %v", err)
		}
	})

	t.Run("the caller gave up part-way through the feed", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(stall)
		defer server.Close()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(50*time.Millisecond, cancel)

		cfg := ingestion.FetchConfig{MaxAttempts: 1, Timeout: time.Minute}
		_, err := cfg.Fetch(ctx, server.URL+"/feed.xml", ingestion.Validators{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		// A half-read document is not a malformed feed, and calling it one
		// would send a human to look at a source that is perfectly fine.
		if errors.Is(err, ingestion.ErrTimeout) {
			t.Errorf("a cancelled read was reported as a timeout: %v", err)
		}
	})

	t.Run("the attempt ran out of time part-way through the feed", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(stall)
		defer server.Close()

		cfg := ingestion.FetchConfig{MaxAttempts: 1, Timeout: 50 * time.Millisecond}
		_, err := cfg.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
		if !errors.Is(err, ingestion.ErrTimeout) {
			t.Fatalf("error = %v, want ErrTimeout", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Errorf("our own deadline was reported as the caller's cancellation: %v", err)
		}
	})

	t.Run("the caller gave up while we were waiting to retry", func(t *testing.T) {
		t.Parallel()

		var hits atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(50*time.Millisecond, cancel)

		// The backoff is long enough that the cancellation certainly lands
		// inside it rather than around it.
		cfg := ingestion.FetchConfig{MaxAttempts: 3, BaseBackoff: 30 * time.Second}
		_, err := cfg.Fetch(ctx, server.URL+"/feed.xml", ingestion.Validators{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		// The source's own last word survives the cancellation: a poll loop
		// deciding what to do with this source next needs to know it was
		// unwell, not merely that a context ended.
		if !errors.Is(err, ingestion.ErrUnavailable) {
			t.Errorf("error = %v, want the source's last failure kept in the chain", err)
		}
		if got := hits.Load(); got != 1 {
			t.Errorf("source was asked %d times, want 1", got)
		}
	})
}

func TestFetchDoesNotRetryA404(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "no such feed")
	}))
	defer server.Close()

	// Three attempts are allowed and a retry would cost thirty seconds, so
	// a fast single request is the assertion.
	cfg := ingestion.FetchConfig{MaxAttempts: 3, BaseBackoff: 30 * time.Second}
	started := time.Now()
	result, err := cfg.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
	elapsed := time.Since(started)

	if !errors.Is(err, ingestion.ErrRefused) {
		t.Fatalf("error = %v, want ErrRefused", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("source was asked %d times, want 1: a missing feed is missing again a second later", got)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the 404 took %v, so it was backed off and retried", elapsed)
	}
	if len(result.Items) != 0 {
		t.Errorf("got %d items from a 404, want none", len(result.Items))
	}
	// The publisher's own words are what tell a missing feed apart from a
	// blocked one, so the error quotes them.
	if !strings.Contains(err.Error(), "no such feed") {
		t.Errorf("the error does not quote what the source said: %v", err)
	}
}

// halfFeedThenRST answers with valid headers and half a document, then
// resets the connection: the failure mode of a proxy restarting mid-transfer.
func halfFeedThenRST(t *testing.T, feed []byte, hits *atomic.Int64) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("test server does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijacking: %v", err)
		}
		half := feed[:len(feed)/2]
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/rss+xml\r\n\r\n")
		_, _ = conn.Write(half)
		// Linger zero turns Close into an RST rather than a FIN: the read
		// side sees a connection error, not a clean end of body.
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}
}

func TestFetchRetriesAConnectionLostMidFeed(t *testing.T) {
	t.Parallel()

	feed := feedFixture(t)
	var hits atomic.Int64
	server := httptest.NewServer(halfFeedThenRST(t, feed, &hits))
	defer server.Close()

	// The same reset one packet earlier - before the headers - is wrapped
	// in ErrUnavailable and retried. Dying mid-body must not be treated
	// differently: it is the transport failing, not the document lying.
	cfg := ingestion.FetchConfig{MaxAttempts: 2, BaseBackoff: time.Millisecond}
	_, err := cfg.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
	if !errors.Is(err, ingestion.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable: a reset mid-body is the source being unwell, not a broken document", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("source was asked %d times, want 2: a transport failure is retried wherever in the exchange it happened", got)
	}
}

func TestFetchClassifiesTheCallersClientTimeoutMidFeed(t *testing.T) {
	t.Parallel()

	feed := feedFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(feed[:len(feed)/2])
		w.(http.Flusher).Flush()
		// Stall until the client gives up: its own timeout closing the
		// connection cancels the request context and releases the handler,
		// so shutting the server down never waits on it.
		<-r.Context().Done()
	}))
	defer server.Close()

	// The caller's own Client.Timeout firing while the body streams lands
	// in the parser's error, not the transport's. It is still a timeout
	// and must be named one - not returned as an unclassified parse error
	// the poll loop would escalate to a human.
	cfg := ingestion.FetchConfig{
		Client:      &http.Client{Timeout: 100 * time.Millisecond},
		MaxAttempts: 1,
	}
	_, err := cfg.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
	if !errors.Is(err, ingestion.ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
}

func TestFetchIgnoresARetryAfterOnARefusal(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "no such feed")
	}))
	defer server.Close()

	// A stray Retry-After on a 404 - misconfigured CDNs send these - is
	// not the source speaking about rate. Reporting it would have the poll
	// loop dutifully deferring a source that actually refused us.
	cfg := ingestion.FetchConfig{MaxAttempts: 1}
	result, err := cfg.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
	if !errors.Is(err, ingestion.ErrRefused) {
		t.Fatalf("error = %v, want ErrRefused", err)
	}
	if result.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want 0: a refusal carries no instruction worth relaying", result.RetryAfter)
	}
}

func TestFetchDoesNotRetryARedirectLoop(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, server.URL+"/feed.xml", http.StatusFound)
	}))
	defer server.Close()

	// A loop is deterministic: walking it a second and third time takes
	// eighteen requests to learn what the first walk already proved. It is
	// a refusal - the source's configuration needs a human.
	cfg := ingestion.FetchConfig{MaxAttempts: 3, BaseBackoff: 30 * time.Second}
	started := time.Now()
	_, err := cfg.Fetch(t.Context(), server.URL+"/feed.xml", ingestion.Validators{})
	if !errors.Is(err, ingestion.ErrRefused) {
		t.Fatalf("error = %v, want ErrRefused", err)
	}
	if got, walkOnce := hits.Load(), int64(6); got > walkOnce {
		t.Errorf("source was asked %d times, want at most %d: the loop was walked more than once", got, walkOnce)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("the loop took %v, so it was backed off and retried", elapsed)
	}
}

func TestFetchErrorsNeverQuoteCredentials(t *testing.T) {
	t.Parallel()

	// Licensed feeds are handed out with basic-auth userinfo in the URL,
	// and every one of these errors is destined for a log line a human
	// reads. The password must not make the trip.
	cases := map[string]string{
		"a scheme this client refuses": "ftp://editor:hunter2@example.org/feed.xml",
		"a URL with no host":           "https://editor:hunter2@/feed.xml",
	}
	for name, feedURL := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var cfg ingestion.FetchConfig
			_, err := cfg.Fetch(t.Context(), feedURL, ingestion.Validators{})
			if !errors.Is(err, ingestion.ErrUnsafeLocation) {
				t.Fatalf("error = %v, want ErrUnsafeLocation", err)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the error quotes the password: %v", err)
			}
		})
	}
}
