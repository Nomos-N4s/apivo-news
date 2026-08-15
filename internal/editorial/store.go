package editorial

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the narrow slice of database access this module needs, defined
// here per the boundary rules (the consumer names its dependency). The
// platform pool satisfies it; the composition root in cmd wires it in.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrDuplicateSourceURL reports a source registration whose feed URL is
// already on record. Handlers map it to 409.
var ErrDuplicateSourceURL = errors.New("editorial: a source with this feed URL already exists")

// ErrUnknownLanguage reports a source registration naming a language that
// is not in the language reference table. Handlers map it to 400.
var ErrUnknownLanguage = errors.New("editorial: unknown language code")

// NewSource is a source registration: the fields an editor supplies. The
// usage rule is deliberately absent - new sources are always
// extract_and_link; upgrades are a founder-gated flow outside this module.
type NewSource struct {
	Name         string
	URL          string
	Language     string
	Jurisdiction string
	LicenceTerms string
}

// Source is a registered source as the database recorded it.
type Source struct {
	ID           uuid.UUID
	Name         string
	URL          string
	Language     string
	Jurisdiction string
	LicenceTerms string
	UsageRule    string
	CreatedAt    time.Time
}

// Store is the module's persistence seam. Handlers depend on it; PGStore
// implements it against Postgres. Tests may substitute it to exercise
// failure paths no real database produces on demand.
type Store interface {
	CreateSource(ctx context.Context, src NewSource) (Source, error)
}

// PGStore is the Postgres-backed Store.
type PGStore struct {
	db DB
}

// NewPGStore builds the Store the composition root wires.
func NewPGStore(db DB) *PGStore { return &PGStore{db: db} }

// CreateSource registers a licensed source. The usage rule is not an
// input: the column's default (extract_and_link) is the only value a
// registration can produce. A duplicate feed URL reports
// ErrDuplicateSourceURL; an unknown language reports ErrUnknownLanguage.
func (s *PGStore) CreateSource(ctx context.Context, src NewSource) (Source, error) {
	var (
		out Source
		id  string
	)
	err := s.db.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, $3, $4, $5)
		 returning id, name, url, language_code, jurisdiction, licence_terms, usage_rule, created_at`,
		src.Name, src.URL, src.Language, src.Jurisdiction, src.LicenceTerms,
	).Scan(&id, &out.Name, &out.URL, &out.Language, &out.Jurisdiction, &out.LicenceTerms, &out.UsageRule, &out.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch {
			case pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == "source_url_key":
				return Source{}, fmt.Errorf("%w: %s", ErrDuplicateSourceURL, src.URL)
			case pgErr.Code == pgerrcode.ForeignKeyViolation && pgErr.ConstraintName == "source_language_code_fkey":
				return Source{}, fmt.Errorf("%w: %q", ErrUnknownLanguage, src.Language)
			}
		}
		return Source{}, fmt.Errorf("editorial: create source: %w", err)
	}
	out.ID, err = uuid.Parse(id)
	if err != nil {
		return Source{}, fmt.Errorf("editorial: parsing created source id: %w", err)
	}
	return out, nil
}
