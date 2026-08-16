package editorial_test

// Contract tests for the opaque cursors themselves: each list accepts only
// the cursors it issued - not the other list's, not oversized ones - and
// answers the documented 400 for everything else.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
)

// TestCursorsAreNotInterchangeableAcrossLists replays each list's genuine
// next_cursor against the other endpoint. Both lists page on a
// (timestamp, id) pair, so an untagged cursor would decode cleanly on the
// wrong list and turn a queue item's retrieved_at into a source-list
// created_at bound - a 200 with rows silently missing where the contract
// promises 400 for a cursor the endpoint did not issue.
func TestCursorsAreNotInterchangeableAcrossLists(t *testing.T) {
	t.Parallel()
	rowID := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	at := time.Date(2026, 8, 14, 9, 0, 0, 123456000, time.UTC)

	t.Run("a queue cursor on the source list is 400", func(t *testing.T) {
		t.Parallel()
		issuing := &queueStore{page: editorial.QueuePage{
			NextCursor: &editorial.QueueCursor{RetrievedAt: at, RowID: rowID},
		}}
		h := editorial.NewHandler(discardLogger(), issuing, fakeAuth{})
		rec, body := getQueue(t, h, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("issuing the queue cursor: status = %d (body %q)", rec.Code, rec.Body.String())
		}
		if body.NextCursor == nil {
			t.Fatal("next_cursor = null, want the queue to issue one")
		}

		// The store must never be reached by a cursor the endpoint refused.
		h = newHandler(t, errStore{err: errUnexpectedCall})
		rec = doJSON(t, h, http.MethodGet, "/api/v1/editorial/sources?cursor="+*body.NextCursor, editorToken, "")
		wantProblem(t, rec, http.StatusBadRequest, "cursor is not one this endpoint issued")
	})

	t.Run("a source cursor on the queue is 400", func(t *testing.T) {
		t.Parallel()
		issuing := &sourcesStore{page: editorial.SourcesPage{
			NextCursor: &editorial.SourceCursor{CreatedAt: at, ID: rowID},
		}}
		h := editorial.NewHandler(discardLogger(), issuing, fakeAuth{})
		rec, body := getSources(t, h, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("issuing the source cursor: status = %d (body %q)", rec.Code, rec.Body.String())
		}
		if body.NextCursor == nil {
			t.Fatal("next_cursor = null, want the source list to issue one")
		}

		h = newHandler(t, errStore{err: errUnexpectedCall})
		rec = doJSON(t, h, http.MethodGet, "/api/v1/editorial/queue?cursor="+*body.NextCursor, editorToken, "")
		wantProblem(t, rec, http.StatusBadRequest, "cursor is not one this endpoint issued")
	})
}

// TestOversizedCursorsAreRejected pins the shared Cursor parameter's
// documented bound: values over 256 characters answer 400 before any
// decoding, on both editorial lists - the same bound the content module
// already enforces for the reader's cursors.
func TestOversizedCursorsAreRejected(t *testing.T) {
	t.Parallel()
	// Valid base64url characters, so only the length can be the offence.
	long := strings.Repeat("A", 257)

	for _, path := range []string{"/api/v1/editorial/queue", "/api/v1/editorial/sources"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			h := newHandler(t, errStore{err: errUnexpectedCall})
			rec := doJSON(t, h, http.MethodGet, path+"?cursor="+long, editorToken, "")
			wantProblem(t, rec, http.StatusBadRequest, "cursor is not one this endpoint issued")
		})
	}
}
