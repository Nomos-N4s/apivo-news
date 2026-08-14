package identity_test

// Token verification is exercised end to end through Authenticate against
// a local JWKS server with freshly generated keys: the happy paths (RS256
// and ES256), the rejection table (expiry, future iat, wrong key, unknown
// kid, garbage, alg=none, HS256 downgrade), and the audience rules.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/Nomos-N4s/apivo-news/internal/identity"
)

// signingKey wraps a raw private key as a jwk.Key carrying kid and alg,
// ready both for signing and for publication in a test JWKS.
func signingKey(t *testing.T, raw any, alg jwa.SignatureAlgorithm, kid string) jwk.Key {
	t.Helper()
	key, err := jwk.Import(raw)
	if err != nil {
		t.Fatalf("importing key: %v", err)
	}
	if err := key.Set(jwk.KeyIDKey, kid); err != nil {
		t.Fatalf("setting kid: %v", err)
	}
	if err := key.Set(jwk.AlgorithmKey, alg); err != nil {
		t.Fatalf("setting alg: %v", err)
	}
	return key
}

// jwksServer serves the public halves of the given keys as a JWKS.
func jwksServer(t *testing.T, keys ...jwk.Key) *httptest.Server {
	t.Helper()
	set := jwk.NewSet()
	for _, k := range keys {
		pub, err := jwk.PublicKeyOf(k)
		if err != nil {
			t.Fatalf("deriving public key: %v", err)
		}
		if err := set.AddKey(pub); err != nil {
			t.Fatalf("adding key to set: %v", err)
		}
	}
	body, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshalling JWKS: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// tokenSpec describes the claims of a token to mint. Zero-valued fields
// are omitted from the token entirely.
type tokenSpec struct {
	sub   string
	iat   time.Time
	exp   time.Time
	aud   []string
	email string
}

// mintToken builds and signs a compact JWT with the given key and claims.
func mintToken(t *testing.T, key jwk.Key, alg jwa.SignatureAlgorithm, spec tokenSpec) string {
	t.Helper()
	b := jwt.NewBuilder()
	if spec.sub != "" {
		b = b.Subject(spec.sub)
	}
	if !spec.iat.IsZero() {
		b = b.IssuedAt(spec.iat)
	}
	if !spec.exp.IsZero() {
		b = b.Expiration(spec.exp)
	}
	if len(spec.aud) > 0 {
		b = b.Audience(spec.aud)
	}
	if spec.email != "" {
		b = b.Claim("email", spec.email)
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatalf("building token: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(alg, key))
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return string(signed)
}

// craftToken hand-assembles a compact serialization from a raw protected
// header, the claims of spec, and signature bytes - shapes no signing
// library should be willing to produce.
func craftToken(t *testing.T, header string, spec tokenSpec, sig []byte) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"sub": spec.sub,
		"iat": spec.iat.Unix(),
		"exp": spec.exp.Unix(),
	})
	if err != nil {
		t.Fatalf("marshalling claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(header)) +
		"." + base64.RawURLEncoding.EncodeToString(payload) +
		"." + base64.RawURLEncoding.EncodeToString(sig)
}

// fakeDB satisfies identity.Querier with a canned account row (or error),
// keeping the token table free of a database dependency. The account
// mapping against the real schema is covered by the integration test.
type fakeDB struct {
	email string
	name  string
	err   error
}

func (f fakeDB) QueryRow(context.Context, string, ...any) pgx.Row { return fakeRow(f) }

type fakeRow fakeDB

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*string)) = r.email
	*(dest[1].(*string)) = r.name
	return nil
}

func TestAuthenticate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	subject := uuid.New()
	now := time.Now()
	fresh := tokenSpec{sub: subject.String(), iat: now.Add(-time.Minute), exp: now.Add(time.Hour)}
	account := fakeDB{email: "row@example.test", name: "Row Name"}

	rsaRaw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating rsa key: %v", err)
	}
	ecRaw, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating ec key: %v", err)
	}
	strangerRaw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating stranger key: %v", err)
	}

	rsaKey := signingKey(t, rsaRaw, jwa.RS256(), "test-rsa")
	ecKey := signingKey(t, ecRaw, jwa.ES256(), "test-ec")
	// The classic downgrade: HS256 with the RSA public key bytes as the
	// HMAC secret, presented under the kid of the real RSA key.
	secret, err := x509.MarshalPKIXPublicKey(&rsaRaw.PublicKey)
	if err != nil {
		t.Fatalf("marshalling public key: %v", err)
	}
	hmacKey := signingKey(t, secret, jwa.HS256(), "test-rsa")

	srv := jwksServer(t, rsaKey, ecKey)

	newVerifier := func(audience string) *identity.Verifier {
		t.Helper()
		v, err := identity.NewVerifier(ctx, identity.VerifierConfig{
			JWKSURL:    srv.URL,
			Audience:   audience,
			HTTPClient: srv.Client(),
		})
		if err != nil {
			t.Fatalf("NewVerifier: %v", err)
		}
		t.Cleanup(func() { _ = v.Close(context.Background()) })
		return v
	}
	anyAudience := newVerifier("")
	authenticated := newVerifier("authenticated")

	errDB := errors.New("connection torn down")

	cases := []struct {
		name     string
		verifier *identity.Verifier
		token    string
		db       fakeDB
		wantErr  error // sentinel matched with errors.Is; nil expects success
		want     identity.Identity
	}{
		{
			name:     "valid RS256 token, email claim wins",
			verifier: anyAudience,
			token: mintToken(t, rsaKey, jwa.RS256(), tokenSpec{
				sub: subject.String(), iat: fresh.iat, exp: fresh.exp, email: "claim@example.test",
			}),
			db:   account,
			want: identity.Identity{Subject: subject, Email: "claim@example.test", DisplayName: "Row Name"},
		},
		{
			name:     "valid ES256 token, email falls back to the account row",
			verifier: anyAudience,
			token:    mintToken(t, ecKey, jwa.ES256(), fresh),
			db:       account,
			want:     identity.Identity{Subject: subject, Email: "row@example.test", DisplayName: "Row Name"},
		},
		{
			name:     "expired token",
			verifier: anyAudience,
			token: mintToken(t, rsaKey, jwa.RS256(), tokenSpec{
				sub: subject.String(), iat: now.Add(-2 * time.Hour), exp: now.Add(-time.Hour),
			}),
			db:      account,
			wantErr: identity.ErrInvalidToken,
		},
		{
			name:     "token issued in the future",
			verifier: anyAudience,
			token: mintToken(t, rsaKey, jwa.RS256(), tokenSpec{
				sub: subject.String(), iat: now.Add(time.Hour), exp: now.Add(2 * time.Hour),
			}),
			db:      account,
			wantErr: identity.ErrInvalidToken,
		},
		{
			name:     "wrong key under a known kid",
			verifier: anyAudience,
			token:    mintToken(t, signingKey(t, strangerRaw, jwa.RS256(), "test-rsa"), jwa.RS256(), fresh),
			db:       account,
			wantErr:  identity.ErrInvalidToken,
		},
		{
			name:     "unknown kid",
			verifier: anyAudience,
			token:    mintToken(t, signingKey(t, strangerRaw, jwa.RS256(), "not-in-set"), jwa.RS256(), fresh),
			db:       account,
			wantErr:  identity.ErrInvalidToken,
		},
		{
			name:     "garbage",
			verifier: anyAudience,
			token:    "definitely.not a.jwt",
			db:       account,
			wantErr:  identity.ErrInvalidToken,
		},
		{
			name:     "alg none",
			verifier: anyAudience,
			token:    craftToken(t, `{"alg":"none","typ":"JWT"}`, fresh, nil),
			db:       account,
			wantErr:  identity.ErrInvalidToken,
		},
		{
			name:     "protected header names no algorithm",
			verifier: anyAudience,
			token:    craftToken(t, `{"typ":"JWT"}`, fresh, []byte("junk")),
			db:       account,
			wantErr:  identity.ErrInvalidToken,
		},
		{
			name:     "HS256 signed with the RSA public key as secret",
			verifier: anyAudience,
			token:    mintToken(t, hmacKey, jwa.HS256(), fresh),
			db:       account,
			wantErr:  identity.ErrInvalidToken,
		},
		{
			name:     "matching audience accepted",
			verifier: authenticated,
			token: mintToken(t, rsaKey, jwa.RS256(), tokenSpec{
				sub: subject.String(), iat: fresh.iat, exp: fresh.exp, aud: []string{"authenticated"},
			}),
			db:   account,
			want: identity.Identity{Subject: subject, Email: "row@example.test", DisplayName: "Row Name"},
		},
		{
			name:     "missing audience rejected when one is required",
			verifier: authenticated,
			token:    mintToken(t, rsaKey, jwa.RS256(), fresh),
			db:       account,
			wantErr:  identity.ErrInvalidToken,
		},
		{
			name:     "wrong audience rejected",
			verifier: authenticated,
			token: mintToken(t, rsaKey, jwa.RS256(), tokenSpec{
				sub: subject.String(), iat: fresh.iat, exp: fresh.exp, aud: []string{"anon"},
			}),
			db:      account,
			wantErr: identity.ErrInvalidToken,
		},
		{
			name:     "audience ignored when none is configured",
			verifier: anyAudience,
			token: mintToken(t, rsaKey, jwa.RS256(), tokenSpec{
				sub: subject.String(), iat: fresh.iat, exp: fresh.exp, aud: []string{"anon"},
			}),
			db:   account,
			want: identity.Identity{Subject: subject, Email: "row@example.test", DisplayName: "Row Name"},
		},
		{
			name:     "valid signature but no subject",
			verifier: anyAudience,
			token:    mintToken(t, rsaKey, jwa.RS256(), tokenSpec{iat: fresh.iat, exp: fresh.exp}),
			db:       account,
			wantErr:  identity.ErrInvalidToken,
		},
		{
			name:     "subject is not a uuid",
			verifier: anyAudience,
			token: mintToken(t, rsaKey, jwa.RS256(), tokenSpec{
				sub: "editor-7", iat: fresh.iat, exp: fresh.exp,
			}),
			db:      account,
			wantErr: identity.ErrInvalidToken,
		},
		{
			name:     "valid token for an unprovisioned subject",
			verifier: anyAudience,
			token:    mintToken(t, rsaKey, jwa.RS256(), fresh),
			db:       fakeDB{err: pgx.ErrNoRows},
			wantErr:  identity.ErrUnknownAccount,
		},
		{
			name:     "database failure is not an auth verdict",
			verifier: anyAudience,
			token:    mintToken(t, rsaKey, jwa.RS256(), fresh),
			db:       fakeDB{err: errDB},
			wantErr:  errDB,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := identity.New(tc.verifier, tc.db)
			got, err := svc.Authenticate(ctx, tc.token)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Authenticate error = %v, want errors.Is(err, %v)", err, tc.wantErr)
				}
				if got != (identity.Identity{}) {
					t.Errorf("Authenticate returned a non-zero identity alongside an error: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if got != tc.want {
				t.Errorf("Authenticate = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAuthenticateErrorsAreDistinguishable(t *testing.T) {
	t.Parallel()
	// Consumers map ErrInvalidToken and ErrUnknownAccount to 401 and a
	// database failure to 500; the sentinels must never alias.
	if errors.Is(identity.ErrInvalidToken, identity.ErrUnknownAccount) ||
		errors.Is(identity.ErrUnknownAccount, identity.ErrInvalidToken) {
		t.Fatal("ErrInvalidToken and ErrUnknownAccount must be distinct sentinels")
	}
}

func TestNewVerifierRequiresJWKSURL(t *testing.T) {
	t.Parallel()
	if _, err := identity.NewVerifier(t.Context(), identity.VerifierConfig{}); err == nil {
		t.Fatal("NewVerifier accepted an empty JWKS URL")
	}
}

func TestNewVerifierRejectsUnregistrableURL(t *testing.T) {
	t.Parallel()
	// A control character makes the URL unparsable, failing registration
	// itself (before any fetch is attempted).
	_, err := identity.NewVerifier(t.Context(), identity.VerifierConfig{
		JWKSURL: "http://example.test/jwks\x01",
	})
	if err == nil {
		t.Fatal("NewVerifier accepted an unparsable JWKS URL")
	}
}

func TestNewVerifierFailsFastOnUnreachableJWKS(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	client := srv.Client()
	srv.Close() // construction must fail at wiring time, not on first use
	_, err := identity.NewVerifier(t.Context(), identity.VerifierConfig{
		JWKSURL:    srv.URL,
		HTTPClient: client,
	})
	if err == nil {
		t.Fatal("NewVerifier succeeded against an unreachable JWKS endpoint")
	}
}
