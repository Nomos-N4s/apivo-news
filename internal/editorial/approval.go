package editorial

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/editorial/store"
)

// Domain event types written alongside the writes they describe. The
// withdrawal event is deliberately absent: migration 0002 writes it by
// trigger, so emitting one here would duplicate the audit record.
const (
	eventArticleApproved  = "article.approved"
	eventArticlePublished = "article.published"
)

// roleEditor is the account.role value that carries editorial authority. It
// mirrors the literal the 0002 triggers and is_entitled check; the database
// remains the authority, this is the same question asked in the same
// transaction.
const roleEditor = "editor"

// ErrOriginAlreadyApproved reports an origin that already has a
// non-withdrawn article. Handlers map it to 409. It comes from the partial
// unique indexes, never from an application pre-check: only the database
// can settle the race between two concurrent approvals.
var ErrOriginAlreadyApproved = errors.New("editorial: origin already has a non-withdrawn article")

// ErrUnknownOrigin reports an approval naming a translation or source item
// that does not exist. Handlers map it to 400.
var ErrUnknownOrigin = errors.New("editorial: named origin does not exist")

// ErrUntitledOrigin reports an untranslated origin whose feed provided no
// title: there would be nothing to render as a headline. Handlers map it
// to 400.
var ErrUntitledOrigin = errors.New("editorial: untranslated origin has no title")

// ErrArticleNotFound reports an article id with no article behind it.
// Handlers map it to 404.
var ErrArticleNotFound = errors.New("editorial: no such article")

// UnknownPlaceError reports an approval naming a place slug the place table
// does not know. Handlers map it to 400, naming the slug the same way the
// reader's front page does - the two endpoints share one place vocabulary.
type UnknownPlaceError struct {
	// Slug is the place slug that names nothing.
	Slug string
}

func (e UnknownPlaceError) Error() string {
	return fmt.Sprintf("editorial: unknown place %q", e.Slug)
}

// ErrAlreadyPublished reports a publication request for an article that is
// already published - the transition is one-way and happens once. Handlers
// map it to 409.
var ErrAlreadyPublished = errors.New("editorial: article is already published")

// NewApproval is an editorial decision to publish: exactly one origin, the
// attribution rendered with the article, and whether publication is
// immediate.
type NewApproval struct {
	// TranslationID is the translation being approved, nil when the origin
	// is an untranslated retrieved item.
	TranslationID *uuid.UUID
	// SourceItemID is the retrieved item being approved untranslated, nil
	// when the origin is a translation.
	SourceItemID *uuid.UUID
	// Attribution is the attribution block pointing back to the original
	// publisher; never blank.
	Attribution string
	// Publish requests immediate publication in the same transaction.
	Publish bool
	// Places names where the article publishes to, as place slugs; never
	// empty. The front page is scoped by place, so an article tagged to no
	// place is one no reader can ever reach - the 0006 constraint trigger
	// makes that state unrepresentable, and this field is how the approval
	// satisfies it.
	Places []string
	// ApprovedBy is the authenticated editor. The database checks the role
	// again on insert (I-1).
	ApprovedBy uuid.UUID
}

// Article is an approved article as the database recorded it.
type Article struct {
	// ID is the article id.
	ID uuid.UUID
	// ApprovedBy is the named editor who approved it (I-1).
	ApprovedBy uuid.UUID
	// ApprovedAt is when the approval was recorded.
	ApprovedAt time.Time
	// PublishedAt is when publication began, nil while the article remains
	// an approved-but-unreleased record.
	PublishedAt *time.Time
}

// approvedEvent is the article.approved audit payload: which article, whose
// decision, when, and the origin it was born from.
type approvedEvent struct {
	ArticleID     string  `json:"article_id"`
	ApprovedBy    string  `json:"approved_by"`
	ApprovedAt    string  `json:"approved_at"`
	TranslationID *string `json:"translation_id"`
	SourceItemID  *string `json:"source_item_id"`
}

// publishedEvent is the article.published audit payload. published_by is
// the editor who released it, which is not an article column - publication
// is a decision, and the audit stream is where it is recorded.
type publishedEvent struct {
	ArticleID   string `json:"article_id"`
	PublishedBy string `json:"published_by"`
	PublishedAt string `json:"published_at"`
}

// Approve creates the article - the approval IS the insert (I-1) - and
// writes its domain events in the same transaction, so an approval whose
// audit record failed to write does not exist at all.
//
// The 403 and 409 verdicts are the database's: the editor-role trigger and
// the one-per-origin partial indexes. This method translates them, and
// deliberately does not pre-empt them - an application pre-check would
// disagree with the database under concurrency, which is exactly when the
// answer matters.
func (s *PGStore) Approve(ctx context.Context, a NewApproval) (Article, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Article{}, fmt.Errorf("editorial: beginning approval: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if a.SourceItemID != nil {
		if err := requireTitle(ctx, q, *a.SourceItemID); err != nil {
			return Article{}, err
		}
	}

	row, err := q.ApproveArticle(ctx, store.ApproveArticleParams{
		TranslationID: optionalUUID(a.TranslationID),
		SourceItemID:  optionalUUID(a.SourceItemID),
		ApprovedBy:    pgtype.UUID{Bytes: a.ApprovedBy, Valid: true},
		Attribution:   a.Attribution,
		Publish:       a.Publish,
	})
	if err != nil {
		return Article{}, approvalError(err)
	}

	// The article and its places are one atomic fact: the 0006 constraint
	// trigger raises at COMMIT unless at least one article_place row exists,
	// so tagging happens here, inside the approving transaction, not after
	// it. The row count is the database's verdict on the slugs - a shortfall
	// means one of them names no place, and the whole approval rolls back.
	tagged, err := q.TagArticlePlaces(ctx, store.TagArticlePlacesParams{
		ArticleID: row.ID,
		Slugs:     a.Places,
	})
	if err != nil {
		return Article{}, fmt.Errorf("editorial: tagging article places: %w", err)
	}
	if tagged < int64(len(a.Places)) {
		return Article{}, unknownPlace(ctx, tx, a.Places)
	}

	article := Article{
		ID:         uuid.UUID(row.ID.Bytes),
		ApprovedBy: uuid.UUID(row.ApprovedBy.Bytes),
		ApprovedAt: row.ApprovedAt.Time,
	}
	if row.PublishedAt.Valid {
		published := row.PublishedAt.Time
		article.PublishedAt = &published
	}

	// Timestamps in audit payloads are normalised to UTC before formatting:
	// pgx decodes timestamptz into the process's local zone, and what lands
	// in the append-only stream is frozen forever - the recorded rendering
	// must not depend on where the server stood.
	if err := recordEvent(ctx, q, eventArticleApproved, approvedEvent{
		ArticleID:     article.ID.String(),
		ApprovedBy:    article.ApprovedBy.String(),
		ApprovedAt:    article.ApprovedAt.UTC().Format(timeFormat),
		TranslationID: uuidText(a.TranslationID),
		SourceItemID:  uuidText(a.SourceItemID),
	}); err != nil {
		return Article{}, err
	}
	if article.PublishedAt != nil {
		if err := recordEvent(ctx, q, eventArticlePublished, publishedEvent{
			ArticleID:   article.ID.String(),
			PublishedBy: article.ApprovedBy.String(),
			PublishedAt: article.PublishedAt.UTC().Format(timeFormat),
		}); err != nil {
			return Article{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Article{}, fmt.Errorf("editorial: committing approval: %w", err)
	}
	return article, nil
}

// Publish releases an approved-but-unpublished article and records the
// article.published event in the same transaction.
//
// Publication is the one editorial write no trigger can guard: nothing on
// the article row records who released it, so the schema cannot know. The
// authority is therefore established here, and established the way the
// 0002 triggers do it - a locking read of the actor's account row, held for
// the rest of the transaction, so a concurrent demotion of that account
// serializes against this publication instead of racing it. The write
// itself carries the editor predicate as well, so it cannot commit without
// one even if this check were somehow bypassed.
func (s *PGStore) Publish(ctx context.Context, articleID, editorID uuid.UUID) (Article, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Article{}, fmt.Errorf("editorial: beginning publication: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := lockEditorRole(ctx, q, editorID); err != nil {
		return Article{}, err
	}

	id := pgtype.UUID{Bytes: articleID, Valid: true}
	row, err := q.PublishArticle(ctx, store.PublishArticleParams{
		ArticleID:   id,
		PublishedBy: pgtype.UUID{Bytes: editorID, Valid: true},
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The guarded UPDATE matched nothing: either there is no such
		// article, or it is already published. Only the database can say
		// which, and it is the same transaction that just tried.
		if _, err := q.ArticleLifecycle(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return Article{}, fmt.Errorf("%w: %s", ErrArticleNotFound, articleID)
		} else if err != nil {
			return Article{}, fmt.Errorf("editorial: reading article lifecycle: %w", err)
		}
		return Article{}, fmt.Errorf("%w: %s", ErrAlreadyPublished, articleID)
	case err != nil:
		return Article{}, fmt.Errorf("editorial: publishing article: %w", err)
	}

	published := row.PublishedAt.Time
	article := Article{
		ID:          uuid.UUID(row.ID.Bytes),
		ApprovedBy:  uuid.UUID(row.ApprovedBy.Bytes),
		ApprovedAt:  row.ApprovedAt.Time,
		PublishedAt: &published,
	}
	if err := recordEvent(ctx, q, eventArticlePublished, publishedEvent{
		ArticleID:   article.ID.String(),
		PublishedBy: editorID.String(),
		PublishedAt: published.UTC().Format(timeFormat),
	}); err != nil {
		return Article{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Article{}, fmt.Errorf("editorial: committing publication: %w", err)
	}
	return article, nil
}

// lockEditorRole takes the row lock on the actor's account and reports
// ErrNotEditor unless it holds the editor role. Both verdicts are the same
// answer to the caller: an account that has been deleted is no more an
// editor than one that was demoted.
//
// The lock is what makes the check worth anything. It is released only when
// the transaction ends, so between here and the commit no demotion of this
// account can slip past: the demoting UPDATE blocks on the share lock, and
// if it committed first this read - being a locking one - sees its result
// rather than a stale snapshot.
func lockEditorRole(ctx context.Context, q *store.Queries, editorID uuid.UUID) error {
	role, err := q.LockActorRole(ctx, pgtype.UUID{Bytes: editorID, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: account %s no longer exists", ErrNotEditor, editorID)
	case err != nil:
		return fmt.Errorf("editorial: locking the actor's role: %w", err)
	case role != roleEditor:
		return fmt.Errorf("%w: account %s holds the %s role", ErrNotEditor, editorID, role)
	}
	return nil
}

// unknownPlace names the slug that made TagArticlePlaces fall short. The
// diagnostic read runs in the same transaction, so it sees exactly the
// place table the insert saw; the error is a 400's worth of precision, not
// a second verdict. Slugs are read fresh rather than derived from the
// insert because :execrows reports only a count.
func unknownPlace(ctx context.Context, tx pgx.Tx, slugs []string) error {
	rows, err := tx.Query(ctx,
		`select slug from place where slug = any ($1::text[])`, slugs)
	if err != nil {
		return fmt.Errorf("editorial: resolving place slugs: %w", err)
	}
	known, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("editorial: reading place slugs: %w", err)
	}
	for _, slug := range slugs {
		if !slices.Contains(known, slug) {
			return UnknownPlaceError{Slug: slug}
		}
	}
	// Fewer rows than slugs, yet every slug resolves: only a duplicate in
	// the list could do that, and the handler rejects duplicates before any
	// write. Whatever happened is not a caller mistake this code can name.
	return errors.New("editorial: tagged fewer places than the approval named")
}

// requireTitle refuses an untranslated origin the reader could not be shown:
// its headline is source_item.original_title, so a feed that provided none
// leaves nothing to render. A whitespace-only title counts as none - the
// column carries no not-blank check, and a blank headline is no headline.
func requireTitle(ctx context.Context, q *store.Queries, sourceItemID uuid.UUID) error {
	title, err := q.SourceItemTitle(ctx, pgtype.UUID{Bytes: sourceItemID, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: source item %s", ErrUnknownOrigin, sourceItemID)
	case err != nil:
		return fmt.Errorf("editorial: reading origin title: %w", err)
	case !title.Valid || blank(title.String):
		return fmt.Errorf("%w: source item %s", ErrUntitledOrigin, sourceItemID)
	}
	return nil
}

// approvalError translates the database's verdicts on an approval into the
// module's errors. Anything it does not recognise stays a failure, not a
// verdict: a rule this code has not been taught must surface as a 500, never
// as a plausible-looking 4xx.
func approvalError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("editorial: approving article: %w", err)
	}
	switch {
	// The one-per-origin partial indexes (0002 shape): they cover
	// non-withdrawn rows only, so a withdrawn origin passes here and the
	// correction flow works.
	case pgErr.Code == pgerrcode.UniqueViolation &&
		(pgErr.ConstraintName == "article_one_per_translation" ||
			pgErr.ConstraintName == "article_one_per_source_item"):
		return fmt.Errorf("%w: %s", ErrOriginAlreadyApproved, pgErr.ConstraintName)
	case pgErr.Code == pgerrcode.ForeignKeyViolation &&
		(pgErr.ConstraintName == "article_translation_id_fkey" ||
			pgErr.ConstraintName == "article_source_item_id_fkey"):
		return fmt.Errorf("%w: %s", ErrUnknownOrigin, pgErr.ConstraintName)
	case databaseRefusedEditor(pgErr):
		return fmt.Errorf("%w: %s", ErrNotEditor, pgErr.Message)
	}
	return fmt.Errorf("editorial: approving article: %w", err)
}

// databaseRefusedEditor reports whether the database refused an editorial
// decision because the account behind it does not hold the editor role -
// article_insert_guard on approval, article_guard on withdrawal.
//
// Matching the rules' shared wording rather than the bare SQLSTATE keeps a
// future rule in the same trigger from being reported as this one; an
// unmatched raise falls through to a 500, which is the safe direction.
func databaseRefusedEditor(pgErr *pgconn.PgError) bool {
	return pgErr.Code == pgerrcode.RaiseException && strings.Contains(pgErr.Message, "editor role")
}

// recordEvent appends one domain event to the append-only audit stream,
// inside the caller's transaction.
func recordEvent(ctx context.Context, q *store.Queries, eventType string, payload any) error {
	blob, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("editorial: encoding %s event: %w", eventType, err)
	}
	if err := q.RecordDomainEvent(ctx, store.RecordDomainEventParams{Type: eventType, Payload: blob}); err != nil {
		return fmt.Errorf("editorial: recording %s event: %w", eventType, err)
	}
	return nil
}

// optionalUUID converts an optional id to the database's nullable form.
func optionalUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

// uuidText renders an optional id for a JSON audit payload, preserving the
// distinction between "this origin" and "not this origin shape".
func uuidText(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}
