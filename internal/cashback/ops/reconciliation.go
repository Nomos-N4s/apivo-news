// Importing a network statement as an immutable run (T110, US6, C-3).
//
// What a statement is, and why it is read in full before it is written, is
// statement.go. This file is the writing: one transaction that records the
// run and announces it, or records nothing. A statement imported twice would
// be two runs, two queues, and one report needing two resolutions before
// FR-043 counts it - so the same statement for the same account and period
// is answered with the run it already produced (0028).

package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

// ImportStatement records a statement as an immutable run, and announces it.
//
// Validation comes first and outside the transaction, because it needs
// nothing from the database and a refused statement should cost no
// connection. Inside it: the account is looked up so a wrong id is answered
// as one; the insert is left to decide whether this statement is already
// here, because two concurrent identical imports would both pass a lookup
// and one would then fail on the constraint; and the event goes in the same
// transaction as the row, so there is no path that records an import nobody
// hears about or announces one the database does not hold. A retry that
// finds its statement already imported appends no event, because the import
// it would describe is already in the stream.
func (s *PGStore) ImportStatement(ctx context.Context, st Statement) (ImportedStatement, error) {
	if err := st.Validate(); err != nil {
		return ImportedStatement{}, err
	}
	lines, err := st.Lines()
	if err != nil {
		return ImportedStatement{}, err
	}
	period := st.Period.normalised()

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return ImportedStatement{}, fmt.Errorf("%w: %w", ErrStatementNotImported, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.New(tx)

	account, err := queries.LookUpNetworkAccount(ctx, pgtype.UUID{Bytes: st.Account, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ImportedStatement{}, fmt.Errorf("%w: %s", ErrNoSuchNetworkAccount, st.Account)
	case err != nil:
		return ImportedStatement{}, fmt.Errorf("%w: looking up account %s: %w", ErrStatementNotImported, st.Account, err)
	}

	params := store.ImportStatementParams{
		NetworkAccountID:     pgtype.UUID{Bytes: st.Account, Valid: true},
		StatementPeriodStart: pgtype.Timestamptz{Time: period.Start, Valid: true},
		StatementPeriodEnd:   pgtype.Timestamptz{Time: period.End, Valid: true},
		ImportedBy:           pgtype.UUID{Bytes: st.Operator.ID, Valid: true},
		RawStatement:         st.Raw,
	}
	imported := ImportedStatement{
		Account: st.Account,
		Network: networks.NetworkID(account.NetworkID),
		Period:  period,
		Lines:   len(lines),
	}
	row, err := queries.ImportStatement(ctx, params)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The constraint spoke: this statement is already here. Read the
		// run it produced, in this transaction, so what is answered is what
		// the insert saw.
		existing, err := queries.ImportedStatement(ctx, store.ImportedStatementParams{
			NetworkAccountID:     params.NetworkAccountID,
			StatementPeriodStart: params.StatementPeriodStart,
			StatementPeriodEnd:   params.StatementPeriodEnd,
			RawStatement:         params.RawStatement,
		})
		if err != nil {
			// Including no rows: an insert refused as a duplicate of a row
			// that cannot be found is a contradiction, not a state to
			// report as success.
			return ImportedStatement{}, fmt.Errorf("%w: the statement is already imported but its run could not be read: %w",
				ErrStatementNotImported, err)
		}
		imported.ID = uuid.UUID(existing.ID.Bytes)
		imported.ImportedBy = uuid.UUID(existing.ImportedBy.Bytes)
		imported.ImportedAt = existing.ImportedAt.Time
		imported.Digest = existing.StatementDigest
		imported.AlreadyImported = true
	case err != nil:
		return ImportedStatement{}, fmt.Errorf("%w: %w", ErrStatementNotImported, err)
	default:
		imported.ID = uuid.UUID(row.ID.Bytes)
		imported.ImportedBy = uuid.UUID(row.ImportedBy.Bytes)
		imported.ImportedAt = row.ImportedAt.Time
		imported.Digest = row.StatementDigest
		payload, err := importedEvent(imported)
		if err != nil {
			return ImportedStatement{}, fmt.Errorf("%w: %w", ErrStatementNotImported, err)
		}
		if _, err := s.events.Append(ctx, tx, events.Message{
			Type:           TypeStatementImported,
			Subject:        imported.ID,
			IdempotencyKey: importedKey(imported.ID),
			Payload:        payload,
		}); err != nil {
			return ImportedStatement{}, fmt.Errorf("%w: %w", ErrStatementNotImported, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return ImportedStatement{}, fmt.Errorf("%w: %w", ErrStatementNotImported, err)
	}
	return imported, nil
}
