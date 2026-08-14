package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrUnknownAccount reports a cryptographically valid token whose subject
// has no account row: the person authenticated with Supabase but was never
// provisioned here. Consumers map it to 401 (the caller holds no account,
// so nothing is authorized).
var ErrUnknownAccount = errors.New("identity: token subject has no account")

// Identity is the authenticated caller of a request.
type Identity struct {
	// Subject is the authenticated account id: the Supabase Auth user id
	// from the token's sub claim, which aligns with account.id (see the
	// account table comment in migration 0001).
	Subject uuid.UUID

	// Email is the caller's email: the token's email claim when present,
	// otherwise the account row's email. Provenance reporting reads email
	// from the account row directly; this field exists for consumers that
	// need the claim without another query.
	Email string

	// DisplayName is the account row's display name - the name that
	// appears as the approver on articles (invariant I-1).
	DisplayName string
}

// Querier is the narrow slice of database access this module needs,
// defined here per the boundary rules (the consumer names its
// dependency). Both *pgxpool.Pool from platform/db and pgx.Tx satisfy it;
// the composition root in cmd wires the pool in.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Service authenticates bearer tokens: signature and claim validation via
// the Verifier, then subject-to-account mapping via the database.
type Service struct {
	verifier *Verifier
	db       Querier
}

// New builds a Service from a constructed Verifier and a database handle.
func New(verifier *Verifier, db Querier) *Service {
	return &Service{verifier: verifier, db: db}
}

// Authenticate verifies the compact JWT in token and resolves its subject
// to an account row. It returns ErrInvalidToken for anything wrong with
// the token itself and ErrUnknownAccount for a valid token whose subject
// is not provisioned; both map to 401. Any other error is a database
// failure, not a verdict about the caller.
func (s *Service) Authenticate(ctx context.Context, token string) (Identity, error) {
	tok, err := s.verifier.verify(ctx, token)
	if err != nil {
		return Identity{}, err
	}
	sub, ok := tok.Subject()
	if !ok || sub == "" {
		return Identity{}, fmt.Errorf("%w: token carries no subject", ErrInvalidToken)
	}
	subject, err := uuid.Parse(sub)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: subject is not a uuid: %w", ErrInvalidToken, err)
	}

	var email, displayName string
	err = s.db.QueryRow(ctx,
		`select email, display_name from account where id = $1`,
		subject.String()).Scan(&email, &displayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, fmt.Errorf("%w: subject %s", ErrUnknownAccount, subject)
	}
	if err != nil {
		return Identity{}, fmt.Errorf("identity: account lookup: %w", err)
	}

	ident := Identity{Subject: subject, Email: email, DisplayName: displayName}
	var claimEmail string
	if err := tok.Get("email", &claimEmail); err == nil && claimEmail != "" {
		ident.Email = claimEmail
	}
	return ident, nil
}
