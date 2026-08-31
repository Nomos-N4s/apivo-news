package clickout_test

// The click and its event as one commit, against the real schema (T076).
//
// Against a real database rather than a fake outbox, because the property
// being asserted is atomicity and atomicity is the database's. A fake could
// only show that this package calls two functions in order, which is not the
// thing that keeps a member from being redirected to a purchase nobody will
// ever hear about.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// clickoutTx opens the outer transaction every case runs a savepoint inside.
// The whole suite rolls back: a click row can never be deleted (C-3), so a
// case that committed would be one every later run carries.
func clickoutTx(t *testing.T) (context.Context, pgx.Tx) {
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

// aClick is one recordable click for the seeded member and offer.
func aClick(t *testing.T, account, offer uuid.UUID) clickout.NewClick {
	t.Helper()
	ref, err := clickout.NewMinter().Mint()
	if err != nil {
		t.Fatalf("Mint(): %v", err)
	}
	return clickout.NewClick{Ref: ref, AccountID: account, OfferID: offer, Promised: aPromise()}
}

// countClicks answers how many click rows the given member has, which is the
// half of atomicity that no amount of reading the outbox can show.
func countClicks(ctx context.Context, t *testing.T, tx pgx.Tx, account uuid.UUID) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx, `select count(*) from cashback.click where account_id = $1`, account).Scan(&n); err != nil {
		t.Fatalf("counting clicks: %v", err)
	}
	return n
}

// TestAClickAndItsEventCommitTogether is the contract in one case: after one
// Record, the row and the event are both there, and the event says what
// contracts/events.md says it says.
func TestAClickAndItsEventCommitTogether(t *testing.T) {
	ctx, tx := clickoutTx(t)
	account, offer := clickable(ctx, t, tx)

	clicks, err := clickout.NewAnnouncedClicks(tx)
	if err != nil {
		t.Fatalf("NewAnnouncedClicks(): %v", err)
	}
	recorded, err := clicks.Record(ctx, aClick(t, account, offer))
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}

	var (
		eventType, producer string
		version             int
		subject, key        *string
		raw                 []byte
	)
	err = tx.QueryRow(ctx, `
		select type, version, producer, subject::text, idempotency_key, payload
		  from domain_event where subject = $1`, recorded.ID.String()).
		Scan(&eventType, &version, &producer, &subject, &key, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the click committed with no event about it")
	}
	if err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}
	if eventType != clickout.TypeClickCreated {
		t.Errorf("type = %q, want %q", eventType, clickout.TypeClickCreated)
	}
	if producer != clickout.EventProducer || version != 1 {
		t.Errorf("producer/version = %q/%d, want %q/1", producer, version, clickout.EventProducer)
	}
	if key == nil || *key != clickout.TypeClickCreated+":"+recorded.ID.String() {
		t.Errorf("idempotency key = %v, want the type and the click", key)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("the payload is not a JSON object: %v", err)
	}
	if payload["click_id"] != recorded.ID.String() {
		t.Errorf("click_id = %v, want %s", payload["click_id"], recorded.ID)
	}
	if payload["account_id"] != account.String() {
		t.Errorf("account_id = %v, want %s", payload["account_id"], account)
	}
	if payload["offer_id"] != offer.String() {
		t.Errorf("offer_id = %v, want %s", payload["offer_id"], offer)
	}
	if payload["at"] != recorded.ClickedAt.UTC().Format("2006-01-02T15:04:05.999999Z07:00") {
		t.Errorf("at = %v, want the instant the row carries (%s)", payload["at"], recorded.ClickedAt.UTC())
	}
	// Consumer rule 5: identifiers travel, data stays in its owning schema.
	// The reference is the secret the redirect is built from and the digest
	// exists for the abuse rules alone; neither belongs in the stream.
	for _, leaked := range []string{"click_ref", "ref", "context_digest", "rate_snapshot", "member_share_bps"} {
		if _, defect := payload[leaked]; defect {
			t.Errorf("the payload carries %q, which stays in the cashback schema", leaked)
		}
	}
}

// TestNoClickSurvivesAnEventThatCouldNotBeAppended is the half that a
// happy-path test cannot show. The outbox is made to refuse, and what must
// be true afterwards is that the member was not redirected to a purchase
// nobody will hear about - so the click is not there either.
func TestNoClickSurvivesAnEventThatCouldNotBeAppended(t *testing.T) {
	ctx, tx := clickoutTx(t)
	account, offer := clickable(ctx, t, tx)

	// A constraint no insert can satisfy, added NOT VALID so the rows
	// already in the stream are left alone. It is the narrowest way to make
	// the append - and only the append - fail against the real schema.
	if _, err := tx.Exec(ctx, `
		alter table public.domain_event
		  add constraint no_events_may_be_appended check (false) not valid`); err != nil {
		t.Fatalf("defeating the outbox: %v", err)
	}

	clicks, err := clickout.NewAnnouncedClicks(tx)
	if err != nil {
		t.Fatalf("NewAnnouncedClicks(): %v", err)
	}
	_, err = clicks.Record(ctx, aClick(t, account, offer))

	if !errors.Is(err, clickout.ErrNotAnnounced) {
		t.Fatalf("Record() error = %v, want one wrapping %v", err, clickout.ErrNotAnnounced)
	}
	if n := countClicks(ctx, t, tx, account); n != 0 {
		t.Errorf("%d click row(s) survived an event that could not be appended, want none", n)
	}
}

// TestNoEventSurvivesAClickThatWasNotRecorded is the same property from the
// other side. A re-used reference is refused by the unique index, and the
// event that would have announced it must go with it.
func TestNoEventSurvivesAClickThatWasNotRecorded(t *testing.T) {
	ctx, tx := clickoutTx(t)
	account, offer := clickable(ctx, t, tx)

	clicks, err := clickout.NewAnnouncedClicks(tx)
	if err != nil {
		t.Fatalf("NewAnnouncedClicks(): %v", err)
	}
	first := aClick(t, account, offer)
	if _, err := clicks.Record(ctx, first); err != nil {
		t.Fatalf("the first click: %v", err)
	}
	if _, err := clicks.Record(ctx, first); !errors.Is(err, clickout.ErrReferenceTaken) {
		t.Fatalf("the second click error = %v, want one wrapping %v", err, clickout.ErrReferenceTaken)
	}

	var events int
	if err := tx.QueryRow(ctx, `
		select count(*) from domain_event
		 where type = $1 and subject in (select id from cashback.click where account_id = $2)`,
		clickout.TypeClickCreated, account).Scan(&events); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if events != 1 {
		t.Errorf("announced %d click(s) for one recorded click, want 1", events)
	}
	if n := countClicks(ctx, t, tx, account); n != 1 {
		t.Errorf("%d click row(s), want the one that was recorded", n)
	}
}

// TestNothingIsAnnouncedAboutAClickTheDatabaseDidNotInsert keeps a fact
// nobody wrote out of the stream. The zero Click is what an unchecked caller
// would pass after a write it did not look at.
func TestNothingIsAnnouncedAboutAClickTheDatabaseDidNotInsert(t *testing.T) {
	ctx, tx := clickoutTx(t)

	announcer, err := clickout.NewAnnouncer()
	if err != nil {
		t.Fatalf("NewAnnouncer(): %v", err)
	}
	if err := announcer.Created(ctx, tx, clickout.Click{}); !errors.Is(err, clickout.ErrNotAnnounced) {
		t.Errorf("Created(zero) = %v, want one wrapping %v", err, clickout.ErrNotAnnounced)
	}
}

// TestARecorderIsRefusedWithNowhereToOpenATransaction. Discovered at
// construction rather than with a member waiting on a redirect.
func TestARecorderIsRefusedWithNowhereToOpenATransaction(t *testing.T) {
	t.Parallel()

	if _, err := clickout.NewAnnouncedClicks(nil); !errors.Is(err, clickout.ErrNoTransactions) {
		t.Errorf("NewAnnouncedClicks(nil) error = %v, want %v", err, clickout.ErrNoTransactions)
	}
}
