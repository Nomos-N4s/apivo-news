package editorial

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/editorial/store"
)

// ErrArticleNotPublished reports a withdrawal request for an article that
// was approved but never released. Withdrawal ends a publication, so there
// is nothing to end; handlers map it to 404, like an unknown article - the
// existence of unpublished work is not something this endpoint confirms.
var ErrArticleNotPublished = errors.New("editorial: article was never published")

// ErrAlreadyWithdrawn reports a withdrawal request for an article whose
// publication has already ended. Withdrawal is one-way and final (I-5), so
// the repeat is a conflict; handlers map it to 409.
var ErrAlreadyWithdrawn = errors.New("editorial: article is already withdrawn")

// Withdraw ends an article's publication, recording who ended it and why.
// Every record survives (FR-016, I-5): the article row, its approval, its
// attribution and the retrieved evidence are untouched.
//
// There is no explicit transaction here, and that is the point: migration
// 0002 writes the article.withdrawn domain event by trigger, in the same
// statement, so the audit record is atomic with the withdrawal by
// construction rather than by application discipline. Emitting an event
// here as well would duplicate it.
func (s *PGStore) Withdraw(ctx context.Context, articleID, editorID uuid.UUID, reason string) (Withdrawal, error) {
	id := pgtype.UUID{Bytes: articleID, Valid: true}
	row, err := s.q.WithdrawArticle(ctx, store.WithdrawArticleParams{
		ArticleID:   id,
		WithdrawnBy: pgtype.UUID{Bytes: editorID, Valid: true},
		Reason:      reason,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The guarded UPDATE matched nothing. Which of the three reasons it
		// was is a question only the database can answer.
		return Withdrawal{}, s.withdrawalRefusal(ctx, articleID, id)
	case err != nil:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && databaseRefusedEditor(pgErr) {
			return Withdrawal{}, fmt.Errorf("%w: %s", ErrNotEditor, pgErr.Message)
		}
		return Withdrawal{}, fmt.Errorf("editorial: withdrawing article: %w", err)
	}

	return Withdrawal{
		ArticleID:   uuid.UUID(row.ID.Bytes),
		WithdrawnAt: row.WithdrawnAt.Time,
		WithdrawnBy: uuid.UUID(row.WithdrawnBy.Bytes),
		Reason:      row.WithdrawalReason.String,
	}, nil
}

// withdrawalRefusal names the reason the guarded update matched no row:
// no such article, an article that was never published, or one whose
// publication has already ended.
func (s *PGStore) withdrawalRefusal(ctx context.Context, articleID uuid.UUID, id pgtype.UUID) error {
	lifecycle, err := s.q.ArticleLifecycle(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrArticleNotFound, articleID)
	case err != nil:
		return fmt.Errorf("editorial: reading article lifecycle: %w", err)
	case !lifecycle.PublishedAt.Valid:
		return fmt.Errorf("%w: %s", ErrArticleNotPublished, articleID)
	}
	// The article is published, so the only predicate left to have failed
	// is `withdrawn_at is null`. That covers the concurrent case too: if a
	// rival withdrawal committed between the two statements, this one lost,
	// and a conflict is exactly what it lost with.
	return fmt.Errorf("%w: %s", ErrAlreadyWithdrawn, articleID)
}
