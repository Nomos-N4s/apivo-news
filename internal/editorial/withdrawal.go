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
// No domain event is written here, deliberately: migration 0002 writes
// article.withdrawn by trigger, in the same statement as the update, so the
// audit record is atomic with the withdrawal by construction rather than by
// application discipline. Emitting one here would duplicate it.
//
// The transaction is not for atomicity, then, but for containment. The
// database refuses a non-editor by raising, and a raise aborts the
// transaction it happened in - so a caller that handed this store a
// transaction of its own would find it poisoned by a refusal it asked for
// and expected to handle. Running the statement in a transaction of this
// method's own confines that to a savepoint the rollback releases.
func (s *PGStore) Withdraw(ctx context.Context, articleID, editorID uuid.UUID, reason string) (Withdrawal, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Withdrawal{}, fmt.Errorf("editorial: beginning withdrawal: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	id := pgtype.UUID{Bytes: articleID, Valid: true}
	row, err := q.WithdrawArticle(ctx, store.WithdrawArticleParams{
		ArticleID:   id,
		WithdrawnBy: pgtype.UUID{Bytes: editorID, Valid: true},
		Reason:      reason,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The guarded UPDATE matched nothing. Which of the three reasons it
		// was is a question only the database can answer.
		return Withdrawal{}, withdrawalRefusal(ctx, q, articleID, id)
	case err != nil:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && databaseRefusedEditor(pgErr) {
			return Withdrawal{}, fmt.Errorf("%w: %s", ErrNotEditor, pgErr.Message)
		}
		return Withdrawal{}, fmt.Errorf("editorial: withdrawing article: %w", err)
	}

	withdrawal := Withdrawal{
		ArticleID:   uuid.UUID(row.ID.Bytes),
		WithdrawnAt: row.WithdrawnAt.Time,
		WithdrawnBy: uuid.UUID(row.WithdrawnBy.Bytes),
		Reason:      row.WithdrawalReason.String,
	}
	if err := tx.Commit(ctx); err != nil {
		return Withdrawal{}, fmt.Errorf("editorial: committing withdrawal: %w", err)
	}
	return withdrawal, nil
}

// withdrawalRefusal names the reason the guarded update matched no row:
// no such article, an article that was never published, or one whose
// publication has already ended.
func withdrawalRefusal(ctx context.Context, q *store.Queries, articleID uuid.UUID, id pgtype.UUID) error {
	lifecycle, err := q.ArticleLifecycle(ctx, id)
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
