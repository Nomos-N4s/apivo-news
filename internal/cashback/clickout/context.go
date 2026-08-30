// Where a click came from, digested (T066, FR-022).
//
// A file of its own because the question it answers is a deployment's, not
// a handler's: which address this process is entitled to believe is the
// client's. Everything else about a click-out is the same wherever it runs.

package clickout

import (
	"net/http"
	"strings"
)

// HandlerOption configures a [Handler].
type HandlerOption func(*Handler)

// WithContextHeader names the header this deployment's edge sets to carry
// the real client address, for the context digest a click records (FR-022).
//
// It is a deployment's statement of trust, and it is deliberately not a
// default. A header a client can set is a context a client can choose, and a
// chosen context evades the per-context half of the click rule by changing
// on every request - so this is only ever the header of an edge that sets it
// itself and strips any inbound copy. Unset, the digest is built from the
// connection's own peer, which behind a proxy is the proxy: still a context,
// and not one that tells devices apart, which is why the composition root
// leaves the per-context rule off unless this is configured.
func WithContextHeader(name string) HandlerOption {
	return func(h *Handler) { h.contextHeader = strings.TrimSpace(name) }
}

// contextOf digests where this request came from (FR-022).
//
// Two parts, and no more than two: the client address as this deployment can
// best determine it, and the user agent. Enough for an abuse rule to tell
// one device's flood from a busy afternoon, and nothing that reconstructs
// who or where somebody is - the digest is what is stored, never these.
//
// The address comes from the configured header when there is one, and from
// the connection otherwise. Only the FIRST value of a multi-valued forwarded
// header is taken: a chain appends, so the leftmost is the client the edge
// saw, and taking the whole chain would make the digest change whenever a
// hop was added.
func (h *Handler) contextOf(r *http.Request) ContextDigest {
	address := r.RemoteAddr
	if h.contextHeader != "" {
		if forwarded := r.Header.Get(h.contextHeader); strings.TrimSpace(forwarded) != "" {
			address, _, _ = strings.Cut(forwarded, ",")
		}
	}
	return NewContextDigest(strings.TrimSpace(address), r.UserAgent())
}
