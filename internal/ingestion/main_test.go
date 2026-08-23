package ingestion_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"testing"
)

// The default client used by these tests gets a connection pool of its own.
//
// Fetch documents a nil Client as meaning http.DefaultClient, and most tests
// here state no client precisely so that documented path is the one under
// test. http.DefaultClient has a nil Transport, so its pool is
// http.DefaultTransport - which every test in this binary would then share.
//
// httptest.Server.Close() ends with, in the standard library's own words,
// "assume most users of httptest.Server will be using the standard
// transport, so help them out and close any idle connections for them":
//
//	if t, ok := http.DefaultTransport.(closeIdleTransport); ok {
//	    t.CloseIdleConnections()
//	}
//
// That courtesy is aimed at the server being closed and cannot be: the pool
// is keyed by address but closed wholesale. These files run around thirty
// parallel subtests, each with a server of its own, each closing at its own
// moment, and every one of those closes reaches into the pool the others are
// still fetching through. A connection torn out from under a request that
// had already been written surfaces to the caller as
//
//	net/http: HTTP/1.x transport connection broken: http: CloseIdleConnections called
//
// wrapped by Fetch as ErrUnavailable - a source that could not be reached,
// reported of a server that answered perfectly. It took a loaded CI runner to
// make it visible, on a subtest asserting ErrRateLimited that got that instead.
//
// So the default client is given a transport that no httptest server will
// ever reach for. Every test keeps exercising the nil-Client path it means
// to; what it no longer shares is a pool with a courtesy-closer.
func TestMain(m *testing.M) {
	http.DefaultClient.Transport = &http.Transport{}
	os.Exit(m.Run())
}

// TestTheDefaultPoolIsNotSharedWithHttptest is the assertion TestMain exists
// for. Without the line above it fails, which is the only way to keep a fix
// for an interleaving nobody can reproduce on demand from quietly rotting.
//
// Sequential on purpose: while a non-parallel top-level test runs, every
// parallel one is paused, so nothing else is opening or closing a server.
func TestTheDefaultPoolIsNotSharedWithHttptest(t *testing.T) {
	mine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer mine.Close()

	get := func() bool {
		reused := false
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, mine.URL, nil)
		if err != nil {
			t.Fatalf("building the request: %v", err)
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
		}))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("asking a server that is up: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatalf("reading the body: %v", err)
		}
		return reused
	}

	get()
	if !get() {
		t.Fatal("the second request opened a new connection: nothing is being pooled, so this test proves nothing")
	}

	// Another test's server, finishing. It knows nothing about this one.
	httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).Close()

	if !get() {
		t.Error("an unrelated server's Close() destroyed this connection: the pool is shared, and a request in flight would have failed instead")
	}
}
