package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/lestrrat-go/httprc/v3"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

// ErrInvalidToken reports a bearer token that failed verification or
// validation: malformed, unsigned, signed with an unaccepted algorithm or
// an unknown/wrong key, expired, issued in the future, or carrying claims
// that do not check out. Consumers map it to 401.
var ErrInvalidToken = errors.New("identity: invalid token")

// VerifierConfig configures a Verifier.
type VerifierConfig struct {
	// JWKSURL is the JWKS endpoint the tokens are verified against - for
	// Supabase, https://<project>.supabase.co/auth/v1/.well-known/jwks.json.
	// Required.
	JWKSURL string

	// Audience, when non-empty, additionally requires every token's aud
	// claim to contain this value. When empty the audience is not checked
	// (Supabase access tokens carry aud "authenticated").
	Audience string

	// HTTPClient overrides the client used to fetch and refresh the JWKS.
	// Optional; when nil the jwx default client (with timeout and redirect
	// protections) is used. Tests point this at a local JWKS server.
	HTTPClient *http.Client
}

// Verifier checks compact JWTs against a cached, auto-refreshing JWKS
// (research D4). Only RS256 and ES256 signatures are accepted: "none" and
// the symmetric HS256 family are rejected before any key is consulted, so
// an attacker cannot downgrade to no signature or reuse a public key as
// an HMAC secret.
type Verifier struct {
	jwksURL  string
	audience string
	cache    *jwk.Cache
}

// NewVerifier builds a Verifier and performs the initial JWKS fetch; an
// unreachable or invalid JWKS endpoint fails construction (fail fast at
// wiring time, not on the first request). The context governs the
// background refresh goroutines: it must stay alive for the lifetime of
// the Verifier, and cancelling it (or calling Close) stops the refreshing.
func NewVerifier(ctx context.Context, cfg VerifierConfig) (*Verifier, error) {
	if cfg.JWKSURL == "" {
		return nil, errors.New("identity: JWKS URL is required")
	}
	cache, err := jwk.NewCache(ctx, httprc.NewClient())
	if err != nil {
		return nil, fmt.Errorf("identity: start JWKS cache: %w", err)
	}
	// Register without waiting: the default wait-until-ready blocks until
	// the first SUCCESSFUL fetch, so against a broken endpoint it would
	// hang instead of erroring. The explicit Refresh below performs the
	// initial fetch synchronously and surfaces the real failure.
	opts := []jwk.RegisterOption{jwk.WithWaitReady(false)}
	if cfg.HTTPClient != nil {
		opts = append(opts, jwk.WithHTTPClient(cfg.HTTPClient))
	}
	if err := cache.Register(ctx, cfg.JWKSURL, opts...); err != nil {
		_ = cache.Shutdown(ctx)
		return nil, fmt.Errorf("identity: register JWKS %q: %w", cfg.JWKSURL, err)
	}
	if _, err := cache.Refresh(ctx, cfg.JWKSURL); err != nil {
		_ = cache.Shutdown(ctx)
		return nil, fmt.Errorf("identity: initial JWKS fetch from %q: %w", cfg.JWKSURL, err)
	}
	return &Verifier{jwksURL: cfg.JWKSURL, audience: cfg.Audience, cache: cache}, nil
}

// Close stops the background JWKS refreshing. The Verifier must not be
// used afterwards.
func (v *Verifier) Close(ctx context.Context) error {
	return v.cache.Shutdown(ctx)
}

// verify checks the compact serialization in raw and returns the parsed,
// validated claims. Signature verification uses the cached key set; claim
// validation covers exp, iat and nbf (and aud when configured).
func (v *Verifier) verify(ctx context.Context, raw string) (jwt.Token, error) {
	data := []byte(raw)
	if err := checkAlgorithms(data); err != nil {
		return nil, err
	}
	set, err := v.cache.Lookup(ctx, v.jwksURL)
	if err != nil {
		return nil, fmt.Errorf("identity: JWKS lookup: %w", err)
	}
	opts := []jwt.ParseOption{
		// Keys are matched by kid/alg; inference lets a JWKS that omits
		// the optional alg member still match. The header allowlist above
		// caps what inference may accept.
		jwt.WithKeySet(set, jws.WithInferAlgorithmFromKey(true)),
	}
	if v.audience != "" {
		opts = append(opts, jwt.WithAudience(v.audience))
	}
	tok, err := jwt.Parse(data, opts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	return tok, nil
}

// checkAlgorithms rejects every token whose protected header names an
// algorithm outside the RS256/ES256 allowlist. Rejecting by header, before
// any verification, makes "alg":"none" and HS256 downgrade attempts fail
// deterministically regardless of key-set contents.
func checkAlgorithms(raw []byte) error {
	msg, err := jws.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	sigs := msg.Signatures()
	if len(sigs) == 0 {
		return fmt.Errorf("%w: token carries no signature", ErrInvalidToken)
	}
	for _, sig := range sigs {
		alg, ok := sig.ProtectedHeaders().Algorithm()
		if !ok {
			return fmt.Errorf("%w: token names no signature algorithm", ErrInvalidToken)
		}
		if alg != jwa.RS256() && alg != jwa.ES256() {
			return fmt.Errorf("%w: algorithm %q is not accepted (RS256 or ES256 only)", ErrInvalidToken, alg)
		}
	}
	return nil
}
