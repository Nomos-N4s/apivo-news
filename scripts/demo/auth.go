//go:build ignore

// A stand-in for the auth provider, for the local demo and nothing else.
//
// WHY THIS EXISTS. Cashback has no anonymous surface (FR-023): the wallet,
// the merchant catalogue, click-outs, participation and the operator queues
// all require a verified bearer token, and the process refuses to mount any
// of them without a JWKS endpoint to verify one against. So a demo that
// cannot issue a token cannot show the product at all - it can only show the
// web app's built-in fixtures beside an API answering 404. That was the
// state this replaces.
//
// WHAT IT STANDS IN FOR. Supabase, in the two roles it plays here: it holds
// the signing key and publishes its public half as a JWKS, and it is where
// an account id comes from. `account.id` IS the Supabase Auth user id, which
// is why `apivo seed cashback` refuses to invent one - so this writes the
// account row too, and the two facts stay one fact.
//
// WHAT IT IS NOT. It is not a login, it is not a Supabase emulator, and it
// must never run anywhere but a developer's own machine. It signs tokens for
// any subject it is asked to, which is the whole point and also the reason
// the build tag above keeps it out of every build, every test binary and
// every image: `go build ./...` does not see this file, and neither does the
// Dockerfile, which builds ./cmd/apivo alone. It runs only when named
// outright: `go run scripts/demo/auth.go`.
//
// Usage:
//
//	go run scripts/demo/auth.go \
//	    -addr 127.0.0.1:8089 \
//	    -database-url "$DATABASE_URL" \
//	    -token-dir /tmp/demo \
//	    -account 00000000-0000-4000-8000-000000000001=reader \
//	    -account 00000000-0000-4000-8000-000000000002=operator
//
// It writes <token-dir>/<role>.token for each account, prints the JWKS URL
// and each account id, and then serves until it is signalled.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

// jwksPath is where the key set is published. The api is told the whole URL,
// so the path is this program's business alone.
const jwksPath = "/jwks.json"

// tokenLifetime is how long a minted token is good for. An hour, because
// that is what Supabase issues and a demo that expired sooner would teach a
// developer to distrust a 401 that was telling the truth.
const tokenLifetime = time.Hour

// account is one identity this program will sign for: the account id, which
// is also the token subject, and the role written to the account row.
type account struct {
	id   uuid.UUID
	role string
}

// accounts collects the repeated -account flag.
type accounts []account

func (a *accounts) String() string { return fmt.Sprint(*a) }

// Set parses <uuid>=<role>. The role doubles as the token's file name, so
// two accounts may not share one: the second would overwrite the first's
// token and the demo would authenticate as the wrong person while every
// individual step reported success.
func (a *accounts) Set(value string) error {
	id, role, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("%q is not <uuid>=<role>", value)
	}
	parsed, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("%q is not an account id: it is the auth user id, a UUID: %w", id, err)
	}
	role = strings.TrimSpace(role)
	if role == "" {
		return fmt.Errorf("%q names no role", value)
	}
	for _, existing := range *a {
		if existing.role == role {
			return fmt.Errorf("role %q is already taken by %s, and the role names the token file", role, existing.id)
		}
	}
	*a = append(*a, account{id: parsed, role: role})
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "demo auth: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr        = flag.String("addr", "127.0.0.1:8089", "address to publish the JWKS on")
		databaseURL = flag.String("database-url", os.Getenv("DATABASE_URL"), "where to write the account rows")
		tokenDir    = flag.String("token-dir", ".", "directory to write <role>.token into")
		who         accounts
	)
	flag.Var(&who, "account", "<uuid>=<role>, repeatable")
	flag.Parse()

	if len(who) == 0 {
		return errors.New("no -account given, so there is nobody to sign for")
	}
	if *databaseURL == "" {
		return errors.New("no -database-url and no DATABASE_URL: the account row is half of what this writes")
	}

	// The listener is opened before anything else is done, because the two
	// remaining failures - a port in use and a database that refuses - are
	// the ones a caller most wants told apart, and a program that wrote
	// rows and then could not listen would leave the first behind.
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *addr, err)
	}
	defer func() { _ = listener.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	key, err := signingKey()
	if err != nil {
		return err
	}
	published, err := publicSet(key)
	if err != nil {
		return err
	}

	if err := writeAccounts(ctx, *databaseURL, who); err != nil {
		return err
	}
	for _, one := range who {
		token, err := mint(key, one.id)
		if err != nil {
			return fmt.Errorf("signing for %s: %w", one.id, err)
		}
		path := filepath.Join(*tokenDir, one.role+".token")
		if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		fmt.Printf("%-10s %s  token in %s\n", one.role, one.id, path)
	}

	url := "http://" + listener.Addr().String() + jwksPath
	fmt.Printf("%-10s %s\n", "jwks", url)

	mux := http.NewServeMux()
	mux.HandleFunc(jwksPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(published)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// signingKey generates an RSA key for this run and only this run. Generated
// rather than read from disk deliberately: a demo key that persisted would
// eventually be a demo key somebody deployed.
func signingKey() (jwk.Key, error) {
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generating a signing key: %w", err)
	}
	key, err := jwk.Import(raw)
	if err != nil {
		return nil, fmt.Errorf("importing the signing key: %w", err)
	}
	if err := key.Set(jwk.KeyIDKey, "demo"); err != nil {
		return nil, err
	}
	if err := key.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		return nil, err
	}
	return key, nil
}

// publicSet is the published half: a JWKS carrying the public key alone.
func publicSet(key jwk.Key) ([]byte, error) {
	pub, err := jwk.PublicKeyOf(key)
	if err != nil {
		return nil, fmt.Errorf("deriving the public key: %w", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(pub); err != nil {
		return nil, fmt.Errorf("building the key set: %w", err)
	}
	body, err := json.Marshal(set)
	if err != nil {
		return nil, fmt.Errorf("encoding the key set: %w", err)
	}
	return body, nil
}

// mint signs an RS256 token for one subject. The claims are the ones the
// verifier actually checks - sub, iat, exp - and nothing more: a token
// carrying claims this deployment ignores would suggest they mattered.
func mint(key jwk.Key, subject uuid.UUID) (string, error) {
	now := time.Now()
	token, err := jwt.NewBuilder().
		Subject(subject.String()).
		IssuedAt(now.Add(-time.Minute)).
		Expiration(now.Add(tokenLifetime)).
		Build()
	if err != nil {
		return "", err
	}
	signed, err := jwt.Sign(token, jwt.WithKey(jwa.RS256(), key))
	if err != nil {
		return "", err
	}
	return string(signed), nil
}

// writeAccounts makes each account exist with the role asked for.
//
// This is the half of Supabase that is not a key: `account.id` is the auth
// user id, and `apivo seed cashback` refuses to invent one precisely so that
// nobody ends up with a member whose id no auth provider ever issued. On a
// developer's machine there is no auth provider to have issued it, so this
// program - which is standing in for exactly that - is the right place for
// the row to come from.
//
// Idempotent, because the demo is run more than once and a second run that
// failed on a duplicate key would be a demo that only worked on a fresh
// database.
func writeAccounts(ctx context.Context, databaseURL string, who accounts) error {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connecting to the database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("reaching the database: %w", err)
	}
	for _, one := range who {
		// The email is derived from the id rather than from the role,
		// because account.email is unique and the role is not: two runs
		// with different member ids and the same role would collide on
		// "reader@demo.invalid" and the second would fail on a constraint
		// naming the email, which says nothing about what went wrong.
		// .invalid is the reserved TLD (RFC 2606), so no address written
		// here can ever reach anybody.
		if _, err := pool.Exec(ctx,
			`insert into public.account (id, email, display_name, role)
			 values ($1, $2, $3, $4)
			 on conflict (id) do update set role = excluded.role`,
			one.id, one.id.String()+"@demo.invalid", "Demo "+strings.ToUpper(one.role[:1])+one.role[1:], one.role,
		); err != nil {
			return fmt.Errorf("writing the %s account: %w", one.role, err)
		}
	}
	return nil
}
