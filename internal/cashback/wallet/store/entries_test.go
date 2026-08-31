package store_test

// What a member's history query returns, and what it must not (T079).
//
// The joins are the risk. Six tables stand between an entry and the name of
// the shop it was earned at, every one of them optional in some legitimate
// case, and an inner join anywhere on that path would silently drop a
// member's own money out of their own wallet.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
)

// shopSourceLanguage is the language every fixture retailer's own copy is
// written in. One value, because what these cases turn on is whether the
// language ASKED FOR is present - a second source language would vary the
// half that is never in question.
const shopSourceLanguage = "de"

// shopName is what every fixture retailer is called in its own language, for
// the reason shopSourceLanguage is one value: the cases turn on WHICH name
// comes back, not on what either says.
const shopName = "Kaufhaus"

// shop seeds a retailer reachable through a network, with copy in its own
// language and optionally in another.
func shop(ctx context.Context, t *testing.T, tx pgx.Tx, also map[string]string) (route pgtype.UUID, networkID string) {
	t.Helper()
	sourceLanguage, name := shopSourceLanguage, shopName
	var merchant pgtype.UUID
	id := tag(t)
	networkID = "walletfix_" + id
	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_second, active)
		values ($1, 'Wallet Fixture Network', 'clickref', 31, 5, true)`, networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.merchant (slug, country, source_language_code, status)
		values ($1, 'DE', $2, 'active') returning id`, "wallet-"+id, sourceLanguage).Scan(&merchant); err != nil {
		t.Fatalf("seeding the merchant: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into cashback.merchant_copy (merchant_id, language_code, name)
		values ($1, $2, $3)`, merchant, sourceLanguage, name); err != nil {
		t.Fatalf("seeding the merchant's own copy: %v", err)
	}
	for language, localised := range also {
		if _, err := tx.Exec(ctx, `
			insert into cashback.merchant_copy (merchant_id, language_code, name)
			values ($1, $2, $3)`, merchant, language, localised); err != nil {
			t.Fatalf("seeding the %s copy: %v", language, err)
		}
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.merchant_network
		    (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload, status, preferred)
		values ('apivo-de', $1, $2, $3, now(), '{"id":"wallet"}'::jsonb, 'active', true) returning id`,
		merchant, networkID, "ext-"+id).Scan(&route); err != nil {
		t.Fatalf("seeding the route: %v", err)
	}
	return route, networkID
}

// earned writes one entry for the member: the report it rests on, the click
// it was attributed through (unless byHand), and the transition that opened
// it carrying the reason.
func earned(ctx context.Context, t *testing.T, tx pgx.Tx, account, route pgtype.UUID, networkID, state, reason string, byHand bool) pgtype.UUID {
	t.Helper()
	return earnedCiting(ctx, t, tx, account, route, networkID, state, reason, byHand, pgtype.UUID{})
}

// earnedCiting is earned, for the case where the entry undoes another.
func earnedCiting(ctx context.Context, t *testing.T, tx pgx.Tx, account, route pgtype.UUID, networkID, state, reason string, byHand bool, reversalOf pgtype.UUID) pgtype.UUID {
	t.Helper()
	id := tag(t)
	var publisher pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active)
		values ($1, $2, 'config:wallet.credential', true) returning id`, networkID, "publisher-"+id).Scan(&publisher); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}
	var offer pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.offer
		    (merchant_network_id, rate_kind, rate_bps, member_share_bps, valid_from, deeplink_template)
		values ($1, 'percent', 500, 6000, now() - interval '1 day', 'https://example.test/d?ref={ref}')
		returning id`, route).Scan(&offer); err != nil {
		t.Fatalf("seeding the offer: %v", err)
	}

	ref := "walletref-" + id
	click := pgtype.UUID{}
	if !byHand {
		if err := tx.QueryRow(ctx, `
			insert into cashback.click
			    (click_ref, account_id, offer_id, rate_snapshot, member_share_bps_snapshot)
			values ($1, $2, $3, '{"kind":"percent","bps":500}'::jsonb, 6000) returning id`,
			ref, account, offer).Scan(&click); err != nil {
			t.Fatalf("seeding the click: %v", err)
		}
	}
	at := purchasedAt
	var report pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end, raw_payload)
		values ($1, $2, $3, $4, 'confirmed', 'confirmed', 4999, 249, 'EUR', $5, $6, $7, $8, '{"id":"wallet"}'::jsonb)
		returning id`,
		networkID, publisher, "EXT-"+id, nullIfByHand(ref, byHand),
		at, at.Add(time.Hour), at.Add(-48*time.Hour), at.Add(48*time.Hour)).Scan(&report); err != nil {
		t.Fatalf("storing the report: %v", err)
	}

	// reversal_of_id is set on INSERT and never after: the entry is frozen
	// once written (C-2, C-3), and a correction is a new row rather than an
	// edit. A fixture that updated it would be testing against a schema
	// nobody deploys.
	var entry pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.entry
		    (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency, reversal_of_id)
		values ('apivo-de', $1, $2, $3, $4, 150, 'EUR', $5) returning id`,
		account, report, click, state, reversalOf).Scan(&entry); err != nil {
		t.Fatalf("seeding the entry: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref, reason)
		values ($1, null, $2, $3, $4)`, entry, state, "transfer-"+id, nullIfBlank(reason)); err != nil {
		t.Fatalf("seeding the opening transition: %v", err)
	}
	return entry
}

// purchasedAt is when every fixture purchase happened.
var purchasedAt = time.Date(2026, time.February, 1, 10, 0, 0, 0, time.UTC)

func nullIfByHand(ref string, byHand bool) any {
	if byHand {
		return nil
	}
	return ref
}

func nullIfBlank(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func listed(ctx context.Context, t *testing.T, tx pgx.Tx, arg store.MemberEntriesParams) []store.MemberEntriesRow {
	t.Helper()
	if arg.PageSize == 0 {
		arg.PageSize = 50
	}
	rows, err := store.New(tx).MemberEntries(ctx, arg)
	if err != nil {
		t.Fatalf("MemberEntries(): %v", err)
	}
	return rows
}

func TestMemberEntriesAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, tx := schemaTx(t)

	each := func(name string, scenario func(t *testing.T, tx pgx.Tx)) {
		t.Run(name, func(t *testing.T) {
			sub, err := tx.Begin(ctx)
			if err != nil {
				t.Fatalf("savepoint: %v", err)
			}
			defer func() { _ = sub.Rollback(ctx) }()
			scenario(t, sub)
		})
	}

	each("an earning carries the purchase and the shop behind it", func(t *testing.T, tx pgx.Tx) {
		account, _ := member(ctx, t, tx)
		route, networkID := shop(ctx, t, tx, map[string]string{"el": "Πολυκατάστημα"})
		earned(ctx, t, tx, account, route, networkID, "confirmed", "the network confirmed it", false)

		rows := listed(ctx, t, tx, store.MemberEntriesParams{AccountID: account, Language: "el"})
		if len(rows) != 1 {
			t.Fatalf("listed %d entries, want 1", len(rows))
		}
		row := rows[0]
		if row.AmountMinor != 150 || row.Currency != "EUR" {
			t.Errorf("the credit is %d %s, want 150 EUR", row.AmountMinor, row.Currency)
		}
		if row.SaleAmountMinor != 4999 || row.SaleCurrency != "EUR" {
			t.Errorf("the purchase is %d %s, want 4999 EUR", row.SaleAmountMinor, row.SaleCurrency)
		}
		if !row.TransactedAt.Time.Equal(purchasedAt) {
			t.Errorf("transacted at %v, want %v", row.TransactedAt.Time, purchasedAt)
		}
		if row.NameInLanguageAsked.String != "Πολυκατάστημα" {
			t.Errorf("the name in Greek is %q, want the Greek copy", row.NameInLanguageAsked.String)
		}
		if row.NameInMerchantsLanguage.String != "Kaufhaus" {
			t.Errorf("the merchant's own name is %q, want Kaufhaus", row.NameInMerchantsLanguage.String)
		}
		if row.Reason.String != "the network confirmed it" {
			t.Errorf("the reason is %q, want the opening transition's", row.Reason.String)
		}
	})

	// US5 scenario 2: no copy in the member's language, so the merchant's
	// own comes back - and the query returns BOTH columns so a caller can
	// tell it apart from a real match.
	each("a shop with no copy in the language asked for falls back", func(t *testing.T, tx pgx.Tx) {
		account, _ := member(ctx, t, tx)
		route, networkID := shop(ctx, t, tx, nil)
		earned(ctx, t, tx, account, route, networkID, "pending", "", false)

		rows := listed(ctx, t, tx, store.MemberEntriesParams{AccountID: account, Language: "el"})
		if len(rows) != 1 {
			t.Fatalf("listed %d entries, want 1", len(rows))
		}
		if rows[0].NameInLanguageAsked.Valid {
			t.Errorf("a Greek name came back (%q) for a shop with no Greek copy", rows[0].NameInLanguageAsked.String)
		}
		if rows[0].NameInMerchantsLanguage.String != "Kaufhaus" {
			t.Errorf("the fallback is %q, want Kaufhaus", rows[0].NameInMerchantsLanguage.String)
		}
		if rows[0].SourceLanguageCode.String != "de" {
			t.Errorf("the fallback's language is %q, want de", rows[0].SourceLanguageCode.String)
		}
	})

	// THE join risk. An operator attributed this by hand because the
	// network named no reference (FR-034), so there is no click - and an
	// inner join anywhere on the path to the shop would drop the member's
	// own money out of their own wallet.
	each("an entry attributed by hand is still the member's", func(t *testing.T, tx pgx.Tx) {
		account, _ := member(ctx, t, tx)
		route, networkID := shop(ctx, t, tx, nil)
		earned(ctx, t, tx, account, route, networkID, "pending", "an operator matched it", true)

		rows := listed(ctx, t, tx, store.MemberEntriesParams{AccountID: account, Language: "de"})
		if len(rows) != 1 {
			t.Fatalf("listed %d entries, want 1: an entry with no click is still an entry", len(rows))
		}
		if rows[0].NameInLanguageAsked.Valid || rows[0].NameInMerchantsLanguage.Valid {
			t.Error("an entry with no click named a shop; there is no route to one")
		}
	})

	each("the state filter narrows, and its absence lists everything", func(t *testing.T, tx pgx.Tx) {
		account, _ := member(ctx, t, tx)
		route, networkID := shop(ctx, t, tx, nil)
		earned(ctx, t, tx, account, route, networkID, "pending", "", false)
		earned(ctx, t, tx, account, route, networkID, "confirmed", "", false)

		all := listed(ctx, t, tx, store.MemberEntriesParams{AccountID: account, Language: "de"})
		if len(all) != 2 {
			t.Fatalf("listed %d entries with no filter, want both", len(all))
		}
		only := listed(ctx, t, tx, store.MemberEntriesParams{
			AccountID: account, Language: "de",
			State: pgtype.Text{String: "confirmed", Valid: true},
		})
		if len(only) != 1 || only[0].State != "confirmed" {
			t.Fatalf("the confirmed filter listed %d entries, want the one confirmed", len(only))
		}
	})

	each("another member's entries are theirs", func(t *testing.T, tx pgx.Tx) {
		account, _ := member(ctx, t, tx)
		other, _ := member(ctx, t, tx)
		route, networkID := shop(ctx, t, tx, nil)
		earned(ctx, t, tx, account, route, networkID, "confirmed", "", false)
		earned(ctx, t, tx, other, route, networkID, "confirmed", "", false)

		rows := listed(ctx, t, tx, store.MemberEntriesParams{AccountID: account, Language: "de"})
		if len(rows) != 1 {
			t.Fatalf("listed %d entries, want only this member's", len(rows))
		}
	})

	// US3 scenario 2, and the property the whole list exists for: a
	// clawback is TWO rows, and neither is hidden. A list that showed only
	// the reversal would look like a credit that never happened, and one
	// that showed only the original would leave a balance nobody could
	// reconcile against it.
	each("a reversal and the credit it undoes both appear", func(t *testing.T, tx pgx.Tx) {
		account, _ := member(ctx, t, tx)
		route, networkID := shop(ctx, t, tx, nil)
		original := earned(ctx, t, tx, account, route, networkID, "confirmed", "the network confirmed it", false)
		earnedCiting(ctx, t, tx, account, route, networkID, "reversed", "the network took it back", false, original)

		rows := listed(ctx, t, tx, store.MemberEntriesParams{AccountID: account, Language: "de"})
		if len(rows) != 2 {
			t.Fatalf("listed %d entries, want both halves of the pair", len(rows))
		}
		var cited bool
		for _, row := range rows {
			if row.State != "reversed" {
				continue
			}
			cited = true
			if row.ReversalOfID != original {
				t.Errorf("the reversal cites %v, want the credit %v", row.ReversalOfID, original)
			}
			if row.Reason.String != "the network took it back" {
				t.Errorf("the reversal's reason is %q, want why the money went back", row.Reason.String)
			}
		}
		if !cited {
			t.Error("the reversal is not in the list; a clawback nobody can see is a balance nobody can check")
		}
	})

	each("newest first, and the cursor resumes after a row", func(t *testing.T, tx pgx.Tx) {
		account, _ := member(ctx, t, tx)
		route, networkID := shop(ctx, t, tx, nil)
		for range 3 {
			earned(ctx, t, tx, account, route, networkID, "confirmed", "", false)
		}

		all := listed(ctx, t, tx, store.MemberEntriesParams{AccountID: account, Language: "de"})
		if len(all) != 3 {
			t.Fatalf("listed %d entries, want 3", len(all))
		}
		for i := 1; i < len(all); i++ {
			previous, current := all[i-1], all[i]
			if current.CreatedAt.Time.After(previous.CreatedAt.Time) {
				t.Fatalf("entry %d is newer than the one before it; the list is not newest-first", i)
			}
		}
		after := listed(ctx, t, tx, store.MemberEntriesParams{
			AccountID: account, Language: "de",
			CursorAt: all[0].CreatedAt, CursorID: all[0].ID,
		})
		if len(after) != 2 {
			t.Fatalf("resuming after the first row listed %d entries, want the remaining 2", len(after))
		}
		if after[0].ID == all[0].ID {
			t.Error("resuming after a row returned that row again")
		}
	})
}
