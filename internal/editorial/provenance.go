package editorial

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Provenance is the full audit chain of one article (I-5, US5): where it
// came from, under which licence terms it was retrieved, what translated
// it and at what cost, who approved it, and how its publication ended if
// it did. Everything an auditor needs, from one query.
type Provenance struct {
	// ArticleID is the article this chain belongs to.
	ArticleID uuid.UUID
	// Headline is what the article is called: the translation's headline,
	// or the retrieved item's original title for an untranslated article.
	Headline string
	// Places is where the article published to, as sorted place slugs;
	// empty only for an article predating the 0006 at-least-one-place rule,
	// which no environment actually has.
	Places []string
	// Source is the feed's identity - and identity ONLY. The legal basis
	// lives in SourceItem's retrieval-time snapshots (I-4), because the
	// source row is mutable and the defence rests on what applied then.
	Source ProvenanceSource
	// SourceItem is the retrieval evidence: what was fetched, when, its
	// database-computed fingerprint, and the licence snapshots.
	SourceItem ProvenanceSourceItem
	// Translation is the machine-translation lineage, nil for an article
	// published untranslated.
	Translation *ProvenanceTranslation
	// Approval names the human whose decision created the article (I-1).
	Approval ProvenanceApproval
	// PublishedAt is when publication began, nil for an approved-but-never-
	// released record.
	PublishedAt *time.Time
	// Withdrawal records how publication ended, nil while it has not.
	Withdrawal *ProvenanceWithdrawal
	// Events is the article's slice of the append-only audit stream,
	// oldest first (FR-012).
	Events []ProvenanceEvent
}

// ProvenanceSource identifies the licensed feed. Identity only - never the
// legal basis (I-4).
type ProvenanceSource struct {
	// Name is the source's display name.
	Name string
	// FeedURL is the feed the crawler polls.
	FeedURL string
	// Jurisdiction is the source's legal jurisdiction.
	Jurisdiction string
}

// ProvenanceSourceItem is the immutable retrieval record (I-2, I-3) with
// its trigger-written licence snapshots (I-4).
type ProvenanceSourceItem struct {
	// SourceURL is the original article at the publisher.
	SourceURL string
	// OriginalTitle is the item title exactly as the feed provided it, nil
	// when the feed omitted one.
	OriginalTitle *string
	// RetrievedAt is when the item was fetched.
	RetrievedAt time.Time
	// ContentHash is the database-computed SHA-256 of the retrieved body.
	ContentHash string
	// LicenceSnapshot is the licence terms as they applied at retrieval.
	LicenceSnapshot string
	// UsageRuleSnapshot is the usage rule in force at retrieval.
	UsageRuleSnapshot string
	// PermissionEvidenceSnapshot is the permission evidence on record at
	// retrieval, nil when there was none.
	PermissionEvidenceSnapshot *string
	// OriginalAuthor is the author as the feed named them, nil when it
	// did not.
	OriginalAuthor *string
}

// ProvenanceTranslation is the lineage of the machine translation the
// article renders (FR-005) and its recorded cost (FR-006).
type ProvenanceTranslation struct {
	// Model is the model that produced the translation.
	Model string
	// PromptVersion is the prompt version in force.
	PromptVersion string
	// TargetLocale is the language translated into.
	TargetLocale string
	// GeneratedAt is when the translation was produced.
	GeneratedAt time.Time
	// CostMicroUSD is the provider-reported cost, recorded at insert.
	CostMicroUSD int64
}

// ProvenanceApproval is the named human decision the article exists by
// (I-1).
type ProvenanceApproval struct {
	// ApproverName is the editor's display name.
	ApproverName string
	// ApproverEmail is the editor's email.
	ApproverEmail string
	// ApprovedAt is when the approval was recorded.
	ApprovedAt time.Time
}

// ProvenanceWithdrawal is the recorded end of publication (FR-016): who,
// when and why, frozen once written.
type ProvenanceWithdrawal struct {
	// WithdrawnAt is when publication ended.
	WithdrawnAt time.Time
	// WithdrawnBy is the account id of the editor who ended it.
	WithdrawnBy uuid.UUID
	// Reason is the recorded justification; never blank.
	Reason string
}

// ProvenanceEvent is one row of the article's audit stream (FR-012).
type ProvenanceEvent struct {
	// Type is the event type, e.g. article.approved.
	Type string
	// OccurredAt is when the event was recorded.
	OccurredAt time.Time
	// Payload is the event's recorded payload, verbatim.
	Payload json.RawMessage
}

// provenanceEventRow is the shape the query's jsonb_agg produces for one
// event; the aggregate arrives as one JSON document.
type provenanceEventRow struct {
	Type       string          `json:"type"`
	OccurredAt time.Time       `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload"`
}

// Provenance answers the audit chain for one article, published or not,
// withdrawn or not - audit sees full history. Unknown ids report
// ErrArticleNotFound.
//
// One statement, one round trip, no transaction: the chain is a single
// read of the article_provenance view (I-5), and the article's domain
// events arrive in the same statement as a jsonb aggregate, so there is no
// second snapshot for them to disagree with.
func (s *PGStore) Provenance(ctx context.Context, articleID uuid.UUID) (Provenance, error) {
	row, err := s.q.GetArticleProvenance(ctx, pgtype.UUID{Bytes: articleID, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Provenance{}, fmt.Errorf("%w: %s", ErrArticleNotFound, articleID)
	case err != nil:
		return Provenance{}, fmt.Errorf("editorial: reading article provenance: %w", err)
	}

	p := Provenance{
		ArticleID: uuid.UUID(row.ArticleID.Bytes),
		Headline:  row.Headline,
		// Never nil: an empty places list renders as [], not null.
		Places: append([]string{}, row.Places...),
		Source: ProvenanceSource{
			Name:         row.SourceName,
			FeedURL:      row.SourceFeedUrl,
			Jurisdiction: row.Jurisdiction,
		},
		SourceItem: ProvenanceSourceItem{
			SourceURL:                  row.SourceUrl,
			OriginalTitle:              textPtr(row.OriginalTitle),
			RetrievedAt:                row.RetrievedAt.Time,
			ContentHash:                row.ContentHash,
			LicenceSnapshot:            row.LicenceSnapshot,
			UsageRuleSnapshot:          row.UsageRuleSnapshot,
			PermissionEvidenceSnapshot: textPtr(row.PermissionEvidenceSnapshot),
			OriginalAuthor:             textPtr(row.OriginalAuthor),
		},
		Approval: ProvenanceApproval{
			ApproverName:  row.ApproverName,
			ApproverEmail: row.ApproverEmail,
			ApprovedAt:    row.ApprovedAt.Time,
		},
	}
	if row.TranslationID.Valid {
		p.Translation = &ProvenanceTranslation{
			Model:         row.Model.String,
			PromptVersion: row.PromptVersion.String,
			TargetLocale:  row.TargetLocale.String,
			GeneratedAt:   row.GeneratedAt.Time,
			CostMicroUSD:  row.CostMicrousd.Int64,
		}
	}
	if row.PublishedAt.Valid {
		published := row.PublishedAt.Time
		p.PublishedAt = &published
	}
	if row.WithdrawnAt.Valid {
		p.Withdrawal = &ProvenanceWithdrawal{
			WithdrawnAt: row.WithdrawnAt.Time,
			WithdrawnBy: uuid.UUID(row.WithdrawnBy.Bytes),
			Reason:      row.WithdrawalReason.String,
		}
	}

	var events []provenanceEventRow
	if err := json.Unmarshal([]byte(row.Events), &events); err != nil {
		return Provenance{}, fmt.Errorf("editorial: decoding provenance events: %w", err)
	}
	p.Events = make([]ProvenanceEvent, 0, len(events))
	for _, e := range events {
		p.Events = append(p.Events, ProvenanceEvent(e))
	}
	return p, nil
}

// Interface satisfaction is asserted where the type is defined so a
// widened Store cannot compile without PGStore following it.
var _ Store = (*PGStore)(nil)
