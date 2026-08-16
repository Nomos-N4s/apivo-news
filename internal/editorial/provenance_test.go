package editorial_test

// Contract tests for GET /api/v1/editorial/articles/{id}/provenance: the
// five-minute audit (US5, FR-010), served from the article_provenance view
// (I-5). Wire shape per specs/001-epiloyes-alpha/contracts/http-api.md and
// the audit screen's TypeScript contract (web/src/lib/editorial/api.ts).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
)

// provenanceStore answers the provenance read with a canned chain and
// records the id the handler asked for.
type provenanceStore struct {
	chain editorial.Provenance
	err   error

	gotArticleID uuid.UUID
}

func (s *provenanceStore) ListSources(context.Context, editorial.SourcesQuery) (editorial.SourcesPage, error) {
	return editorial.SourcesPage{}, errUnexpectedCall
}

func (s *provenanceStore) UpdateSource(context.Context, uuid.UUID, uuid.UUID, editorial.SourcePatch) (editorial.ListedSource, error) {
	return editorial.ListedSource{}, errUnexpectedCall
}

func (s *provenanceStore) DeleteSource(context.Context, uuid.UUID) error {
	return errUnexpectedCall
}

func (s *provenanceStore) LastPollCycle(context.Context) (editorial.PollCycle, error) {
	return editorial.PollCycle{}, errUnexpectedCall
}

func (s *provenanceStore) CreateSource(context.Context, editorial.NewSource) (editorial.Source, error) {
	return editorial.Source{}, errUnexpectedCall
}

func (s *provenanceStore) ReviewQueue(context.Context, editorial.QueueQuery) (editorial.QueuePage, error) {
	return editorial.QueuePage{}, errUnexpectedCall
}

func (s *provenanceStore) Approve(context.Context, editorial.NewApproval) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s *provenanceStore) Publish(context.Context, uuid.UUID, uuid.UUID) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s *provenanceStore) Withdraw(context.Context, uuid.UUID, uuid.UUID, string) (editorial.Withdrawal, error) {
	return editorial.Withdrawal{}, errUnexpectedCall
}

func (s *provenanceStore) Provenance(_ context.Context, articleID uuid.UUID) (editorial.Provenance, error) {
	s.gotArticleID = articleID
	return s.chain, s.err
}

// provenanceBody is the decoded 200 payload, kept deliberately close to
// the contract's wire shape so a renamed field fails the test.
type provenanceBody struct {
	ArticleID string   `json:"article_id"`
	Headline  string   `json:"headline"`
	Places    []string `json:"places"`
	Source    struct {
		Name         string `json:"name"`
		FeedURL      string `json:"feed_url"`
		Jurisdiction string `json:"jurisdiction"`
	} `json:"source"`
	SourceItem struct {
		SourceURL                  string  `json:"source_url"`
		OriginalTitle              *string `json:"original_title"`
		RetrievedAt                string  `json:"retrieved_at"`
		ContentHash                string  `json:"content_hash"`
		LicenceSnapshot            string  `json:"licence_snapshot"`
		UsageRuleSnapshot          string  `json:"usage_rule_snapshot"`
		PermissionEvidenceSnapshot *string `json:"permission_evidence_snapshot"`
		OriginalAuthor             *string `json:"original_author"`
	} `json:"source_item"`
	Translation *struct {
		Model         string `json:"model"`
		PromptVersion string `json:"prompt_version"`
		TargetLocale  string `json:"target_locale"`
		GeneratedAt   string `json:"generated_at"`
		CostMicroUSD  int64  `json:"cost_microusd"`
	} `json:"translation"`
	Approval struct {
		ApproverName  string `json:"approver_name"`
		ApproverEmail string `json:"approver_email"`
		ApprovedAt    string `json:"approved_at"`
	} `json:"approval"`
	PublishedAt *string `json:"published_at"`
	Withdrawal  *struct {
		WithdrawnAt string `json:"withdrawn_at"`
		WithdrawnBy string `json:"withdrawn_by"`
		Reason      string `json:"reason"`
	} `json:"withdrawal"`
	Events []struct {
		Type       string `json:"type"`
		OccurredAt string `json:"occurred_at"`
		Detail     string `json:"detail"`
	} `json:"events"`
}

// fullChain is a translated, published, withdrawn article with events: the
// densest legal shape the endpoint renders.
func fullChain() editorial.Provenance {
	title := "Original title"
	author := "A. Reporter"
	evidence := "written permission of 2026-08-01"
	published := time.Date(2026, 8, 14, 6, 31, 40, 0, time.UTC)
	return editorial.Provenance{
		ArticleID: uuid.MustParse("8f1b6c1e-1f5a-4a2f-9e1a-2b3c4d5e6f70"),
		Headline:  "Übersetzte Überschrift",
		Places:    []string{"bavaria", "munich"},
		Source: editorial.ProvenanceSource{
			Name:         "Munich Feed",
			FeedURL:      "https://example.test/feed.xml",
			Jurisdiction: "DE",
		},
		SourceItem: editorial.ProvenanceSourceItem{
			SourceURL:                  "https://example.test/articles/1",
			OriginalTitle:              &title,
			RetrievedAt:                time.Date(2026, 8, 14, 6, 12, 4, 0, time.UTC),
			ContentHash:                "abc123",
			LicenceSnapshot:            "Extract and link permitted per feed terms v1",
			UsageRuleSnapshot:          "full_text",
			PermissionEvidenceSnapshot: &evidence,
			OriginalAuthor:             &author,
		},
		Translation: &editorial.ProvenanceTranslation{
			Model:         "translate-alpha-1",
			PromptVersion: "v4",
			TargetLocale:  "de",
			GeneratedAt:   time.Date(2026, 8, 14, 6, 14, 22, 0, time.UTC),
			CostMicroUSD:  4100,
		},
		Approval: editorial.ProvenanceApproval{
			ApproverName:  "Contract Editor",
			ApproverEmail: "editor@example.test",
			ApprovedAt:    time.Date(2026, 8, 14, 6, 31, 9, 0, time.UTC),
		},
		PublishedAt: &published,
		Withdrawal: &editorial.ProvenanceWithdrawal{
			WithdrawnAt: time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC),
			WithdrawnBy: uuid.MustParse("00000000-0000-4000-8000-000000000001"),
			Reason:      "the source retracted the story",
		},
		Events: []editorial.ProvenanceEvent{
			{
				Type:       "article.approved",
				OccurredAt: time.Date(2026, 8, 14, 6, 31, 9, 0, time.UTC),
				Payload:    json.RawMessage(`{"article_id":"8f1b6c1e-1f5a-4a2f-9e1a-2b3c4d5e6f70","approved_by":"00000000-0000-4000-8000-000000000001"}`),
			},
			{
				Type:       "article.withdrawn",
				OccurredAt: time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC),
				Payload:    json.RawMessage(`{"article_id":"8f1b6c1e-1f5a-4a2f-9e1a-2b3c4d5e6f70","reason":"the source retracted the story"}`),
			},
			// A payload that is not an object: the fallback answers the
			// recorded bytes verbatim, because the record is the detail even
			// when it has an unexpected shape.
			{
				Type:       "source.note",
				OccurredAt: time.Date(2026, 8, 15, 9, 30, 0, 0, time.UTC),
				Payload:    json.RawMessage(`"free text"`),
			},
		},
	}
}

func TestProvenanceAuth(t *testing.T) {
	t.Parallel()
	h := newHandler(t, errStore{err: errors.New("store must not be reached")})
	path := "/api/v1/editorial/articles/" + uuid.NewString() + "/provenance"

	t.Run("401 without a token", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodGet, path, "", "")
		wantProblem(t, rec, http.StatusUnauthorized, "bearer token is required")
	})
	t.Run("403 for a non-editor", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodGet, path, readerToken, "")
		wantProblem(t, rec, http.StatusForbidden, "editor role")
	})
}

func TestProvenanceValidation(t *testing.T) {
	t.Parallel()
	// The store must never be reached by a malformed id.
	h := newHandler(t, errStore{err: errors.New("store must not be reached")})

	t.Run("400 on a non-uuid id", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/articles/not-a-uuid/provenance", editorToken, "")
		wantProblem(t, rec, http.StatusBadRequest, "must be a uuid")
	})
}

func TestProvenanceNotFound(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &provenanceStore{err: editorial.ErrArticleNotFound})
	rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/articles/"+uuid.NewString()+"/provenance", editorToken, "")
	wantProblem(t, rec, http.StatusNotFound, "no article with this id")
}

func TestProvenanceStoreFailure(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &provenanceStore{err: errors.New("connection torn down")})
	rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/articles/"+uuid.NewString()+"/provenance", editorToken, "")
	wantProblem(t, rec, http.StatusInternalServerError, "")
	if got := rec.Body.String(); len(got) > 0 && json.Valid(rec.Body.Bytes()) {
		var p map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
		if detail, ok := p["detail"].(string); ok && detail != "" {
			t.Errorf("internal error detail leaked to the wire: %q", detail)
		}
	}
}

func TestProvenanceResponseShape(t *testing.T) {
	t.Parallel()
	store := &provenanceStore{chain: fullChain()}
	h := newHandler(t, store)
	id := store.chain.ArticleID.String()

	rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/articles/"+id+"/provenance", editorToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if store.gotArticleID.String() != id {
		t.Errorf("store asked for %s, want %s", store.gotArticleID, id)
	}

	var got provenanceBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshalling 200 body: %v", err)
	}
	if got.ArticleID != id {
		t.Errorf("article_id = %q, want %q", got.ArticleID, id)
	}
	if got.Headline != "Übersetzte Überschrift" {
		t.Errorf("headline = %q", got.Headline)
	}
	if len(got.Places) != 2 || got.Places[0] != "bavaria" || got.Places[1] != "munich" {
		t.Errorf("places = %v, want [bavaria munich]", got.Places)
	}
	if got.Source.Name != "Munich Feed" || got.Source.FeedURL != "https://example.test/feed.xml" || got.Source.Jurisdiction != "DE" {
		t.Errorf("source = %+v", got.Source)
	}
	if got.SourceItem.LicenceSnapshot != "Extract and link permitted per feed terms v1" {
		t.Errorf("licence_snapshot = %q", got.SourceItem.LicenceSnapshot)
	}
	if got.SourceItem.UsageRuleSnapshot != "full_text" {
		t.Errorf("usage_rule_snapshot = %q", got.SourceItem.UsageRuleSnapshot)
	}
	if got.SourceItem.PermissionEvidenceSnapshot == nil || *got.SourceItem.PermissionEvidenceSnapshot != "written permission of 2026-08-01" {
		t.Errorf("permission_evidence_snapshot = %v", got.SourceItem.PermissionEvidenceSnapshot)
	}
	if got.Translation == nil {
		t.Fatal("translation is null for a translated article")
	}
	if got.Translation.Model != "translate-alpha-1" || got.Translation.PromptVersion != "v4" || got.Translation.CostMicroUSD != 4100 {
		t.Errorf("translation = %+v", got.Translation)
	}
	if got.Approval.ApproverName != "Contract Editor" || got.Approval.ApproverEmail != "editor@example.test" {
		t.Errorf("approval = %+v", got.Approval)
	}
	if got.PublishedAt == nil || *got.PublishedAt != "2026-08-14T06:31:40Z" {
		t.Errorf("published_at = %v", got.PublishedAt)
	}
	if got.Withdrawal == nil || got.Withdrawal.Reason != "the source retracted the story" {
		t.Fatalf("withdrawal = %+v", got.Withdrawal)
	}
	if got.Withdrawal.WithdrawnBy != "00000000-0000-4000-8000-000000000001" {
		t.Errorf("withdrawn_by = %q", got.Withdrawal.WithdrawnBy)
	}
	if len(got.Events) != 3 {
		t.Fatalf("events = %d rows, want 3", len(got.Events))
	}
	if got.Events[0].Type != "article.approved" || got.Events[1].Type != "article.withdrawn" {
		t.Errorf("event types = %q, %q", got.Events[0].Type, got.Events[1].Type)
	}
	// The detail is the recorded payload minus the article_id the screen is
	// already scoped to - the record, never a paraphrase.
	if want := `{"approved_by":"00000000-0000-4000-8000-000000000001"}`; got.Events[0].Detail != want {
		t.Errorf("events[0].detail = %q, want %q", got.Events[0].Detail, want)
	}
	if want := `{"reason":"the source retracted the story"}`; got.Events[1].Detail != want {
		t.Errorf("events[1].detail = %q, want %q", got.Events[1].Detail, want)
	}
	// A non-object payload passes through verbatim - the raw record, quotes
	// included, never a paraphrase and never a 500.
	if want := `"free text"`; got.Events[2].Detail != want {
		t.Errorf("events[2].detail = %q, want %q", got.Events[2].Detail, want)
	}

	// The full field set, so an accidentally added or dropped top-level key
	// fails loudly.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("re-unmarshalling body: %v", err)
	}
	want := []string{"article_id", "headline", "places", "source", "source_item", "translation", "approval", "published_at", "withdrawal", "events"}
	if len(keys) != len(want) {
		t.Errorf("response has %d fields, want %d: %v", len(keys), len(want), keys)
	}
	for _, k := range want {
		if _, ok := keys[k]; !ok {
			t.Errorf("response lacks the %q field", k)
		}
	}
}

// TestProvenanceUntranslatedShape pins the nulls: an untranslated,
// unpublished, unwithdrawn article answers explicit nulls for translation,
// published_at and withdrawal, and [] - never null - for places and events.
func TestProvenanceUntranslatedShape(t *testing.T) {
	t.Parallel()
	title := "As the feed titled it"
	chain := editorial.Provenance{
		ArticleID: uuid.MustParse("d57b1f30-6c92-4a44-b8e1-95ac2f7d0e63"),
		Headline:  title,
		Places:    nil,
		Source: editorial.ProvenanceSource{
			Name:         "Athens Feed",
			FeedURL:      "https://example.test/gr.xml",
			Jurisdiction: "GR",
		},
		SourceItem: editorial.ProvenanceSourceItem{
			SourceURL:         "https://example.test/articles/2",
			OriginalTitle:     &title,
			RetrievedAt:       time.Date(2026, 8, 14, 5, 0, 0, 0, time.UTC),
			ContentHash:       "def456",
			LicenceSnapshot:   "Terms v2",
			UsageRuleSnapshot: "extract_and_link",
		},
		Approval: editorial.ProvenanceApproval{
			ApproverName:  "Contract Editor",
			ApproverEmail: "editor@example.test",
			ApprovedAt:    time.Date(2026, 8, 14, 6, 0, 0, 0, time.UTC),
		},
	}
	h := newHandler(t, &provenanceStore{chain: chain})

	rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/articles/"+chain.ArticleID.String()+"/provenance", editorToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("unmarshalling body: %v", err)
	}
	for _, field := range []string{"translation", "published_at", "withdrawal"} {
		if string(keys[field]) != "null" {
			t.Errorf("%s = %s, want an explicit null", field, keys[field])
		}
	}
	for _, field := range []string{"places", "events"} {
		if string(keys[field]) != "[]" {
			t.Errorf("%s = %s, want [] - an empty list, never null", field, keys[field])
		}
	}
	var got provenanceBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshalling body: %v", err)
	}
	if got.Headline != title {
		t.Errorf("headline = %q, want the original title %q", got.Headline, title)
	}
	if got.SourceItem.PermissionEvidenceSnapshot != nil {
		t.Errorf("permission_evidence_snapshot = %v, want null", got.SourceItem.PermissionEvidenceSnapshot)
	}
}
