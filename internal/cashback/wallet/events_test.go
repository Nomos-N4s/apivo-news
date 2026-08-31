package wallet_test

// The two participation facts, against the real outbox (T080).
//
// Against a real database rather than a fake writer, because what is being
// asserted is what the stream will hold and what a second append does to
// the transaction underneath it - and a fake could only show that this
// package calls a function.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// outboxTx opens the outer transaction every case runs a savepoint inside.
// The whole suite rolls back, so a run appends nothing a later one would
// read.
func outboxTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the outbox")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback(ctx)
		pool.Close()
	})
	return ctx, tx
}

func announcer(t *testing.T) *wallet.Announcer {
	t.Helper()
	made, err := wallet.NewAnnouncer()
	if err != nil {
		t.Fatalf("NewAnnouncer(): %v", err)
	}
	return made
}

// joinedAt is one acceptance, dated.
func joinedAt(member uuid.UUID, at time.Time) wallet.Participation {
	return wallet.Participation{
		Member:       member,
		Brand:        "a-brand",
		OptedInAt:    at,
		TermsVersion: "3.1.0",
		Status:       wallet.StatusActive,
		Currency:     "SEK",
	}
}

// participationPayload reads back the one event of the given type about the
// given member, as the fields the contract names.
func participationPayload(ctx context.Context, t *testing.T, tx pgx.Tx, eventType string, member uuid.UUID) struct {
	AccountID    uuid.UUID `json:"account_id"`
	TermsVersion string    `json:"terms_version"`
	At           time.Time `json:"at"`
} {
	t.Helper()
	var raw []byte
	if err := tx.QueryRow(ctx,
		`select payload from domain_event where type = $1 and subject = $2`,
		eventType, member.String()).Scan(&raw); err != nil {
		t.Fatalf("reading the %s payload: %v", eventType, err)
	}
	var payload struct {
		AccountID    uuid.UUID `json:"account_id"`
		TermsVersion string    `json:"terms_version"`
		At           time.Time `json:"at"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decoding the %s payload: %v", eventType, err)
	}
	return payload
}

func TestStartedCarriesTheAcceptance(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := uuid.New()
	at := time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)

	if err := announcer(t).Started(ctx, tx, joinedAt(member, at)); err != nil {
		t.Fatalf("Started(): %v", err)
	}
	payload := participationPayload(ctx, t, tx, wallet.TypeParticipationStarted, member)
	switch {
	case payload.AccountID != member:
		t.Errorf("account_id = %s, want %s", payload.AccountID, member)
	case payload.TermsVersion != "3.1.0":
		t.Errorf("terms_version = %q, want 3.1.0", payload.TermsVersion)
	case !payload.At.Equal(at):
		t.Errorf("at = %v, want the row's own %v", payload.At, at)
	}
}

func TestEndedCarriesTheDeparture(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := uuid.New()
	gone := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	left := joinedAt(member, gone.Add(-time.Hour))
	left.Status, left.LeftAt = wallet.StatusLeft, gone
	if err := announcer(t).Ended(ctx, tx, left); err != nil {
		t.Fatalf("Ended(): %v", err)
	}
	payload := participationPayload(ctx, t, tx, wallet.TypeParticipationEnded, member)
	switch {
	case payload.AccountID != member:
		t.Errorf("account_id = %s, want %s", payload.AccountID, member)
	case payload.TermsVersion != "3.1.0":
		t.Errorf("terms_version = %q; a departure says what they had agreed to", payload.TermsVersion)
	case !payload.At.Equal(gone):
		t.Errorf("at = %v, want the leaving date %v", payload.At, gone)
	}
}

// A member may join, leave and join again, and the second acceptance is a
// fact in its own right. Both cases are announced AT THE SAME INSTANT here,
// which is the shape every candidate idempotency key got wrong: the account
// alone silences the re-join, and the account with opted_in_at collides -
// and a collision is a unique violation, which aborts the transaction, so
// the member's legitimate re-join would have failed with a 500. Two events
// at one instant is therefore the property, not an artefact of the test.
func TestARejoinIsAnnouncedAsWell(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := uuid.New()
	first := time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)

	made := announcer(t)
	if err := made.Started(ctx, tx, joinedAt(member, first)); err != nil {
		t.Fatalf("the first acceptance: %v", err)
	}
	again := joinedAt(member, first)
	again.TermsVersion = "4.0.0"
	if err := made.Started(ctx, tx, again); err != nil {
		t.Fatalf("the second acceptance at the same instant: %v", err)
	}

	var n int
	if err := tx.QueryRow(ctx,
		`select count(*) from domain_event where type = $1 and subject = $2`,
		wallet.TypeParticipationStarted, member.String()).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 2 {
		t.Errorf("the stream holds %d acceptances, want 2", n)
	}
}

// The same departure, twice, at one instant - the case the removed key
// would have turned into an aborted transaction. Nothing deduplicates here
// and nothing needs to: the statement that closes a participation narrows
// on status = 'active', so it closes one exactly once and this is only ever
// reached by the request that did.
func TestTwoDeparturesAtOneInstantBothReachTheStream(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := uuid.New()
	gone := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	left := joinedAt(member, gone.Add(-time.Hour))
	left.Status, left.LeftAt = wallet.StatusLeft, gone

	made := announcer(t)
	for i := range 2 {
		if err := made.Ended(ctx, tx, left); err != nil {
			t.Fatalf("departure %d: %v", i+1, err)
		}
	}
	var n int
	if err := tx.QueryRow(ctx,
		`select count(*) from domain_event where type = $1 and subject = $2`,
		wallet.TypeParticipationEnded, member.String()).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 2 {
		t.Errorf("the stream holds %d departures, want 2", n)
	}
}

// What the announcer refuses outright: facts about rows the database cannot
// have written. Refused before any database work, so each case runs without
// a transaction at all.
func TestAnnouncingRefusesWhatTheSchemaCouldNotHold(t *testing.T) {
	t.Parallel()
	made := announcer(t)
	sound := joinedAt(uuid.New(), time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC))

	t.Run("an acceptance by nobody", func(t *testing.T) {
		t.Parallel()
		nobody := sound
		nobody.Member = uuid.Nil
		if err := made.Started(context.Background(), nil, nobody); !errors.Is(err, wallet.ErrNotAnnounced) {
			t.Fatalf("Started() returned %v, want wallet.ErrNotAnnounced", err)
		}
	})
	t.Run("a departure with no date", func(t *testing.T) {
		t.Parallel()
		undated := sound
		undated.Status = wallet.StatusLeft
		if err := made.Ended(context.Background(), nil, undated); !errors.Is(err, wallet.ErrNotAnnounced) {
			t.Fatalf("Ended() returned %v, want wallet.ErrNotAnnounced", err)
		}
	})
}
