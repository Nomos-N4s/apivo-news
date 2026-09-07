package openbao_test

// A stand-in OpenBao, good enough to hold this adapter to the wire it
// actually speaks.
//
// It models the KV v2 write endpoint the way the server documents it, not
// the way this adapter wishes it were: the path is `/v1/<mount>/data/<key>`,
// the payload nests the caller's fields under `data`, and an unauthenticated
// write is refused rather than quietly accepted. Each of those has been the
// shape of a real mistake in an adapter somewhere, and a fake that forgave
// them would let this one ship with the same one.
//
// The real vault is exercised separately, keyed on an address, exactly as
// the ledger's suites are. This file is what keeps the adapter honest where
// no vault is reachable.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// storedSecret is one write the fake accepted.
type storedSecret struct {
	Path    string
	Token   string
	Kind    string
	Details json.RawMessage
}

// fakeVault is an OpenBao that records what it was asked to store.
type fakeVault struct {
	server *httptest.Server

	mu      sync.Mutex
	written []storedSecret

	// status, when set, is answered instead of accepting the write, so a
	// case can drive the failure paths without a broken server.
	status int
	// requireToken refuses an unauthenticated write, as a real vault does.
	requireToken bool
}

// newFakeVault starts one and stops it with the test.
func newFakeVault(t *testing.T) *fakeVault {
	t.Helper()
	f := &fakeVault{requireToken: true}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeVault) URL() string { return f.server.URL }

func (f *fakeVault) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.status != 0 {
		w.WriteHeader(f.status)
		// A real vault names the path it refused. The adapter must not
		// forward this body, and a case asserts it does not.
		_, _ = w.Write([]byte(`{"errors":["permission denied on ` + r.URL.Path + `"]}`))
		return
	}
	token := r.Header.Get("X-Vault-Token")
	if f.requireToken && token == "" {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["missing client token"]}`))
		return
	}
	// KV v2 writes go to /v1/<mount>/data/<key>. Anything else is this
	// adapter addressing an engine that is not the one it thinks.
	const marker = "/data/"
	if !strings.HasPrefix(r.URL.Path, "/v1/") || !strings.Contains(r.URL.Path, marker) {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var body struct {
		Data struct {
			Kind    string          `json:"kind"`
			Details json.RawMessage `json:"details"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.written = append(f.written, storedSecret{
		Path:    r.URL.Path,
		Token:   token,
		Kind:    body.Data.Kind,
		Details: body.Data.Details,
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"data":{"created_time":"2026-09-07T00:00:00Z","version":1}}`))
}

// stored answers everything written so far.
func (f *fakeVault) stored() []storedSecret {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]storedSecret(nil), f.written...)
}

// answers makes every later write fail with the given status.
func (f *fakeVault) answers(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}
