package blnk_test

// The Blnk ledger against a real Blnk, keyed on BLNK_URL exactly as the
// schema invariant suites are keyed on DATABASE_URL: expected to skip on
// the founder's machine while Docker Desktop is unavailable, and never
// skipped in the cashback CI job, which is the verification of record
// (ADR-0002, spike S3).
//
// This is where the adapter's reading of the substrate is checked against
// the substrate. The fake in fake_test.go proves the requests are built the
// way this repository believes Blnk wants them; only these tests can prove
// that belief was right - that a precise amount lands as the integer it
// was, that a duplicate reference is refused, that a source with nothing in
// it is refused, and that what Post recorded is readable when Post returns.
//
// The ledger outlives a test run and its transactions are immutable by
// design, so nothing here cleans up after itself: every test works with
// accounts and idempotency keys carrying a random suffix, which keeps its
// money separate from every other test's and from every earlier run's.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/blnk"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// liveLedger builds a ledger against the running Blnk, or skips.
//
// Each test gets its own Blnk ledger, named with a random suffix, so one
// test's accounts cannot be another's however alike their references look.
func liveLedger(t *testing.T) *blnk.Ledger {
	t.Helper()
	return liveLedgerNamed(t, "cashback-test-"+suffix(t))
}

// liveLedgerNamed builds a ledger against a named Blnk ledger, so a test can
// build two values over one ledger and prove they agree.
func liveLedgerNamed(t *testing.T, name string) *blnk.Ledger {
	t.Helper()

	endpoint := os.Getenv("BLNK_URL")
	if endpoint == "" {
		t.Skip("BLNK_URL is unset: no ledger to post to (expected without Docker)")
	}
	ledger, err := blnk.New(endpoint,
		blnk.WithSecretKey(os.Getenv("BLNK_SECRET_KEY")),
		blnk.WithLedgerName(name),
		// Short next to any test deadline and long next to what a
		// loopback ledger actually takes, so a wedged endpoint fails here
		// in seconds with a legible message rather than stalling the job.
		blnk.WithTimeout(10*time.Second),
		blnk.WithSettleWindow(20*time.Second, 50*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("building a ledger against %s: %v", endpoint, err)
	}
	return ledger
}

// suffix mints a random tag so one run cannot see another's money.
func suffix(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("minting a test suffix: %v", err)
	}
	return hex.EncodeToString(buf)
}

// liveAccounts is the cast every test below works with: a house account
// money enters the closed set over, and a member's two stage accounts.
type liveAccounts struct {
	ledger    *blnk.Ledger
	house     wallet.LedgerAccountID
	confirmed wallet.LedgerAccountID
	reserved  wallet.LedgerAccountID
}

func liveSetup(t *testing.T) liveAccounts {
	t.Helper()
	ledger := liveLedger(t)
	memberID := uuid.New()
	return liveAccounts{
		ledger:    ledger,
		house:     ensure(t, ledger, wallet.HouseAccount("commission-"+suffix(t)), eur),
		confirmed: ensure(t, ledger, wallet.MemberAccount(memberID, wallet.StageConfirmed), eur),
		reserved:  ensure(t, ledger, wallet.MemberAccount(memberID, wallet.StageReserved), eur),
	}
}

// fund moves minor units from the house account into the member's confirmed
// bucket, which is how money enters the closed set of accounts.
func (a liveAccounts) fund(t *testing.T, key string, minor int64) wallet.TransferRef {
	t.Helper()
	ref, err := a.ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: key,
		Reference:      "funding-" + key,
		Postings: []wallet.Posting{
			{Account: a.house, Amount: amount(-minor, eur)},
			{Account: a.confirmed, Amount: amount(minor, eur)},
		},
	})
	if err != nil {
		t.Fatalf("funding the member with %d: %v", minor, err)
	}
	return ref
}

// TestLiveEnsureAccountIsIdempotent holds the account identity the whole
// adapter rests on against the real server: the same reference and currency
// resolve to the same balance, and a different one never does.
func TestLiveEnsureAccountIsIdempotent(t *testing.T) {
	t.Parallel()

	name := "cashback-test-" + suffix(t)
	ledger := liveLedgerNamed(t, name)
	memberID := uuid.New()
	ref := wallet.MemberAccount(memberID, wallet.StageConfirmed)

	first := ensure(t, ledger, ref, eur)
	if again := ensure(t, ledger, ref, eur); again != first {
		t.Fatalf("one reference resolved to %q and then to %q", first, again)
	}

	// A second ledger value over the same ledger - a second process, as
	// far as anything here can tell - must resolve the same account. The
	// derivation is the identity; the memo only saves a round trip, and a
	// derivation that needed the memo would strand every restart.
	if again := ensure(t, liveLedgerNamed(t, name), ref, eur); again != first {
		t.Fatalf("a second ledger value resolved %q, want the same account %q", again, first)
	}

	for _, pair := range []struct {
		name string
		ref  wallet.AccountRef
		cur  money.Currency
	}{
		{name: "another currency", ref: ref, cur: gbp},
		{name: "another stage", ref: wallet.MemberAccount(memberID, wallet.StageReserved), cur: eur},
		{name: "another member", ref: wallet.MemberAccount(uuid.New(), wallet.StageConfirmed), cur: eur},
		{name: "a house account", ref: wallet.HouseAccount("rounding-" + suffix(t)), cur: eur},
	} {
		if got := ensure(t, ledger, pair.ref, pair.cur); got == first {
			t.Errorf("%s resolved to the same balance %q", pair.name, got)
		}
	}
}

// TestLiveEnsureAccountIsIdempotentUnderConcurrency is the port's racing
// promise against the real server.
func TestLiveEnsureAccountIsIdempotentUnderConcurrency(t *testing.T) {
	t.Parallel()

	ledger := liveLedger(t)
	ref := wallet.MemberAccount(uuid.New(), wallet.StageConfirmed)

	const callers = 6
	ids := make([]wallet.LedgerAccountID, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ids[i], errs[i] = ledger.EnsureAccount(context.Background(), ref, eur)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("caller %d resolved %q, caller 0 resolved %q; one reference must be one account", i, ids[i], ids[0])
		}
	}
}

// TestLivePostIsReadableWhenItReturns is the read-your-writes case, and the
// one that decides whether the whole wallet can be trusted: a member's
// total is recomputed from the ledger straight after a transition, so a
// Post that returned before its money was visible would show the total from
// before it.
//
// It also proves the amount survived the boundary. Blnk carries a float
// amount beside the integer one and this adapter sends only the integer; if
// that were wrong, the balance below would not be the number that was
// posted.
func TestLivePostIsReadableWhenItReturns(t *testing.T) {
	t.Parallel()

	accounts := liveSetup(t)
	key := "readable-" + suffix(t)

	ref := accounts.fund(t, key, 250075)
	if ref == "" {
		t.Fatal("Post returned a blank reference")
	}

	held, err := accounts.ledger.Balance(context.Background(), accounts.confirmed, eur)
	if err != nil {
		t.Fatalf("reading the balance straight after the post: %v", err)
	}
	if !held.Equal(amount(250075, eur)) {
		t.Fatalf("the member holds %s immediately after the post, want %s: the transfer was not readable when Post returned", held, amount(250075, eur))
	}

	// The house account is where the money came from, so it is short by
	// exactly what the member gained - which is C-1 over the pair.
	owed, err := accounts.ledger.Balance(context.Background(), accounts.house, eur)
	if err != nil {
		t.Fatalf("reading the house balance: %v", err)
	}
	if !owed.Equal(amount(-250075, eur)) {
		t.Fatalf("the house account holds %s, want %s", owed, amount(-250075, eur))
	}

	// And the postings are readable too, carrying the reference Post
	// answered with.
	postings := readHistory(t, accounts.ledger, accounts.confirmed, wallet.Window{})
	if len(postings) != 1 {
		t.Fatalf("the member's history holds %d posting(s) immediately after the post, want 1", len(postings))
	}
	if postings[0].Amount.Minor != 250075 {
		t.Errorf("the posting moves %d, want 250075", postings[0].Amount.Minor)
	}
	if postings[0].TransferRef != ref {
		t.Errorf("the posting carries reference %q, want the one Post answered with, %q", postings[0].TransferRef, ref)
	}
	if postings[0].PostedAt.IsZero() {
		t.Error("the posting carries no instant")
	}
}

// TestLivePostReplaysAndConflicts is the idempotency contract against the
// real server: the same key with the same transfer moves money once, and
// the same key with a different transfer is refused rather than quietly
// honoured.
func TestLivePostReplaysAndConflicts(t *testing.T) {
	t.Parallel()

	accounts := liveSetup(t)
	key := "replay-" + suffix(t)
	transfer := wallet.Transfer{
		IdempotencyKey: key,
		Reference:      "entry-" + key,
		Metadata:       map[string]string{"entry": key},
		Postings: []wallet.Posting{
			{Account: accounts.house, Amount: amount(-4000, eur)},
			{Account: accounts.confirmed, Amount: amount(4000, eur)},
		},
	}

	first, err := accounts.ledger.Post(context.Background(), transfer)
	if err != nil {
		t.Fatalf("the first post: %v", err)
	}

	// The same movements, rebuilt in the other order: representation, not
	// identity, so this is a replay.
	reordered := transfer
	reordered.Postings = []wallet.Posting{
		{Account: accounts.confirmed, Amount: amount(4000, eur)},
		{Account: accounts.house, Amount: amount(-4000, eur)},
	}
	again, err := accounts.ledger.Post(context.Background(), reordered)
	if err != nil {
		t.Fatalf("the replay was refused: %v", err)
	}
	if again != first {
		t.Fatalf("the replay answered %q, want the original %q", again, first)
	}

	held, err := accounts.ledger.Balance(context.Background(), accounts.confirmed, eur)
	if err != nil {
		t.Fatalf("reading the balance: %v", err)
	}
	if !held.Equal(amount(4000, eur)) {
		t.Fatalf("the member holds %s after one key was posted twice, want %s", held, amount(4000, eur))
	}

	for _, tc := range []struct {
		name    string
		mutate  func(wallet.Transfer) wallet.Transfer
		wantErr error
	}{
		{
			name: "different amounts",
			mutate: func(w wallet.Transfer) wallet.Transfer {
				w.Postings = []wallet.Posting{
					{Account: accounts.house, Amount: amount(-4001, eur)},
					{Account: accounts.confirmed, Amount: amount(4001, eur)},
				}
				return w
			},
			wantErr: wallet.ErrIdempotencyConflict,
		},
		{
			name: "another domain reference",
			mutate: func(w wallet.Transfer) wallet.Transfer {
				w.Reference = "entry-something-else"
				return w
			},
			wantErr: wallet.ErrIdempotencyConflict,
		},
		{
			name: "another annotation",
			mutate: func(w wallet.Transfer) wallet.Transfer {
				w.Metadata = map[string]string{"entry": "somebody else"}
				return w
			},
			wantErr: wallet.ErrIdempotencyConflict,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := accounts.ledger.Post(context.Background(), tc.mutate(transfer))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Post error = %v, want one wrapping %v", err, tc.wantErr)
			}
		})
	}

	if got := readHistory(t, accounts.ledger, accounts.confirmed, wallet.Window{}); len(got) != 1 {
		t.Errorf("the member's history holds %d posting(s) after one key and three refusals, want 1", len(got))
	}
}

// TestLiveConcurrentPostsOfOneKeyRecordOnce is the crash-recovery case S2
// tests from the outside, asked of the port: several dispatchers reach one
// undispatched instruction at once, and exactly one movement of money must
// result - with every caller learning the same reference, because each of
// them is about to store it as proof that the money moved (D7).
func TestLiveConcurrentPostsOfOneKeyRecordOnce(t *testing.T) {
	t.Parallel()

	accounts := liveSetup(t)
	key := "raced-" + suffix(t)
	transfer := wallet.Transfer{
		IdempotencyKey: key,
		Reference:      "entry-" + key,
		Postings: []wallet.Posting{
			{Account: accounts.house, Amount: amount(-1500, eur)},
			{Account: accounts.confirmed, Amount: amount(1500, eur)},
		},
	}

	const dispatchers = 4
	refs := make([]wallet.TransferRef, dispatchers)
	errs := make([]error, dispatchers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range dispatchers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			refs[i], errs[i] = accounts.ledger.Post(context.Background(), transfer)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("dispatcher %d was refused, but every one of them posted the same transfer under the same key: %v", i, err)
		}
		if refs[i] == "" {
			t.Fatalf("dispatcher %d learned a blank reference", i)
		}
		if refs[i] != refs[0] {
			t.Fatalf("dispatcher %d learned %q and dispatcher 0 learned %q; one key is one transfer", i, refs[i], refs[0])
		}
	}

	held, err := accounts.ledger.Balance(context.Background(), accounts.confirmed, eur)
	if err != nil {
		t.Fatalf("reading the balance: %v", err)
	}
	if !held.Equal(amount(1500, eur)) {
		t.Fatalf("the member holds %s after %d concurrent posts of one key, want %s", held, dispatchers, amount(1500, eur))
	}
	if got := readHistory(t, accounts.ledger, accounts.confirmed, wallet.Window{}); len(got) != 1 {
		t.Fatalf("the member's history holds %d posting(s) after %d concurrent posts of one key, want 1", len(got), dispatchers)
	}
}

// TestLiveMemberAccountCannotGoNegative is the double-spend defence D9
// leans on. Two withdrawal requests race for one confirmed balance; both
// try to reserve it; the ledger is what makes exactly one of them win.
//
// Nothing else in the design refuses the second - there is no
// one-open-request constraint on withdrawal_request - so if this test ever
// fails, members can be paid twice for money they have once.
func TestLiveMemberAccountCannotGoNegative(t *testing.T) {
	t.Parallel()

	accounts := liveSetup(t)
	tag := suffix(t)
	accounts.fund(t, "fund-"+tag, 5000)

	// One plain overdraw first: the refusal is the port's error, not the
	// substrate's.
	_, err := accounts.ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "overdraw-" + tag,
		Postings: []wallet.Posting{
			{Account: accounts.confirmed, Amount: amount(-5001, eur)},
			{Account: accounts.reserved, Amount: amount(5001, eur)},
		},
	})
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("reserving more than the member holds gave %v, want one wrapping %v", err, wallet.ErrInsufficientFunds)
	}

	// Then the race: two reservations for the whole balance, at once.
	const requests = 2
	errs := make([]error, requests)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = accounts.ledger.Post(context.Background(), wallet.Transfer{
				IdempotencyKey: fmt.Sprintf("reserve-%s-%d", tag, i),
				Reference:      "withdrawal-request",
				Postings: []wallet.Posting{
					{Account: accounts.confirmed, Amount: amount(-5000, eur)},
					{Account: accounts.reserved, Amount: amount(5000, eur)},
				},
			})
		}()
	}
	close(start)
	wg.Wait()

	won, refused := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, wallet.ErrInsufficientFunds):
			refused++
		default:
			t.Fatalf("request %d failed for a reason that is not a refusal: %v", i, err)
		}
	}
	if won != 1 || refused != requests-1 {
		t.Fatalf("%d of %d concurrent reservations were accepted, want exactly 1", won, requests)
	}

	held, err := accounts.ledger.Balance(context.Background(), accounts.confirmed, eur)
	if err != nil {
		t.Fatalf("reading the balance: %v", err)
	}
	if held.IsNegative() {
		t.Fatalf("the member's confirmed account holds %s; a stage account may never go below nothing", held)
	}
	if !held.Equal(amount(0, eur)) {
		t.Errorf("the member's confirmed account holds %s after one reservation of the whole balance, want %s", held, amount(0, eur))
	}
}

// TestLiveHouseAccountMayGoNegative is the exemption that makes the ledger
// usable at all: the house account is the boundary of the closed set of
// accounts, and a ledger in which nothing may go negative has nothing able
// to fund its first credit.
func TestLiveHouseAccountMayGoNegative(t *testing.T) {
	t.Parallel()

	accounts := liveSetup(t)
	accounts.fund(t, "house-negative-"+suffix(t), 12345)

	held, err := accounts.ledger.Balance(context.Background(), accounts.house, eur)
	if err != nil {
		t.Fatalf("reading the house balance: %v", err)
	}
	if !held.Equal(amount(-12345, eur)) {
		t.Fatalf("the house account holds %s, want %s", held, amount(-12345, eur))
	}
}

// TestLiveReversalReturnsTheMoney is the entry lifecycle's other end: a
// reversal is the same movement with its amounts flipped, under a key of
// its own, and it must leave both accounts exactly where they started.
func TestLiveReversalReturnsTheMoney(t *testing.T) {
	t.Parallel()

	accounts := liveSetup(t)
	tag := suffix(t)
	accounts.fund(t, "fund-"+tag, 7700)

	reversal, err := accounts.ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "reverse-" + tag,
		Reference:      "reversal-of-fund-" + tag,
		Postings: []wallet.Posting{
			{Account: accounts.confirmed, Amount: amount(-7700, eur)},
			{Account: accounts.house, Amount: amount(7700, eur)},
		},
	})
	if err != nil {
		t.Fatalf("reversing: %v", err)
	}
	if reversal == "" {
		t.Fatal("the reversal returned a blank reference")
	}

	for id, want := range map[wallet.LedgerAccountID]money.Amount{
		accounts.confirmed: amount(0, eur),
		accounts.house:     amount(0, eur),
	} {
		held, err := accounts.ledger.Balance(context.Background(), id, eur)
		if err != nil {
			t.Fatalf("reading %q: %v", id, err)
		}
		if !held.Equal(want) {
			t.Errorf("account %q holds %s after the reversal, want %s", id, held, want)
		}
	}

	// Both movements are still in the record: a reversal undoes a balance,
	// never a history.
	postings := readHistory(t, accounts.ledger, accounts.confirmed, wallet.Window{})
	if len(postings) != 2 {
		t.Fatalf("the member's history holds %d posting(s) after a credit and its reversal, want 2", len(postings))
	}
	if postings[0].Amount.Minor != 7700 || postings[1].Amount.Minor != -7700 {
		t.Errorf("the history reads %s then %s, want +7700 then -7700", postings[0].Amount, postings[1].Amount)
	}
	if postings[1].TransferRef != reversal {
		t.Errorf("the reversing posting carries reference %q, want %q", postings[1].TransferRef, reversal)
	}
}

// TestLiveManyToManyIsOneTransfer is the shape this ledger claims it can
// record atomically, held to the claim: several accounts giving and several
// receiving, in one currency, as one movement - and every posting of it
// readable afterwards under one reference.
//
// The history assertions are the important ones. The adapter finds a
// posting by the account being one end of a recorded transaction, which is
// what a split leg is; if this substrate recorded a split some other way, a
// member's history would silently omit legs while Balance went on being
// right, and this is where that shows up.
func TestLiveManyToManyIsOneTransfer(t *testing.T) {
	t.Parallel()

	ledger := liveLedger(t)
	tag := suffix(t)
	memberID := uuid.New()
	commission := ensure(t, ledger, wallet.HouseAccount("commission-"+tag), eur)
	float := ensure(t, ledger, wallet.HouseAccount("float-"+tag), eur)
	pending := ensure(t, ledger, wallet.MemberAccount(memberID, wallet.StagePending), eur)
	rounding := ensure(t, ledger, wallet.HouseAccount("rounding-"+tag), eur)

	ref, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "split-" + tag,
		Reference:      "commission-split-" + tag,
		Postings: []wallet.Posting{
			{Account: commission, Amount: amount(-700, eur)},
			{Account: float, Amount: amount(-300, eur)},
			{Account: pending, Amount: amount(999, eur)},
			{Account: rounding, Amount: amount(1, eur)},
		},
	})
	if err != nil {
		t.Fatalf("posting a many-to-many transfer: %v", err)
	}

	for id, want := range map[wallet.LedgerAccountID]money.Amount{
		commission: amount(-700, eur),
		float:      amount(-300, eur),
		pending:    amount(999, eur),
		rounding:   amount(1, eur),
	} {
		held, err := ledger.Balance(context.Background(), id, eur)
		if err != nil {
			t.Fatalf("reading %q: %v", id, err)
		}
		if !held.Equal(want) {
			t.Errorf("account %q holds %s, want %s", id, held, want)
		}
	}

	for id, want := range map[wallet.LedgerAccountID]int64{
		commission: -700, float: -300, pending: 999, rounding: 1,
	} {
		postings := readHistory(t, ledger, id, wallet.Window{})
		if len(postings) != 1 {
			t.Errorf("account %q has %d posting(s) from a many-to-many transfer, want 1", id, len(postings))
			continue
		}
		if postings[0].Amount.Minor != want {
			t.Errorf("account %q recorded %d, want %d", id, postings[0].Amount.Minor, want)
		}
		if postings[0].TransferRef != ref {
			t.Errorf("account %q recorded reference %q, want the transfer's own %q", id, postings[0].TransferRef, ref)
		}
	}

	// A split is the one shape where the reference a second caller learns
	// could differ from the first's: if the ledger answers the key with a
	// leg rather than with the transfer, the two callers store different
	// references for one movement and the audit join breaks (D7).
	replay, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "split-" + tag,
		Reference:      "commission-split-" + tag,
		Postings: []wallet.Posting{
			{Account: commission, Amount: amount(-700, eur)},
			{Account: float, Amount: amount(-300, eur)},
			{Account: pending, Amount: amount(999, eur)},
			{Account: rounding, Amount: amount(1, eur)},
		},
	})
	if err != nil {
		t.Fatalf("replaying the key of a split transfer: %v", err)
	}
	if replay != ref {
		t.Errorf("replaying a split transfer's key answered %q, want the transfer's own reference %q", replay, ref)
	}
	_, err = ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "split-" + tag,
		Reference:      "commission-split-" + tag,
		Postings: []wallet.Posting{
			{Account: commission, Amount: amount(-1000, eur)},
			{Account: pending, Amount: amount(1000, eur)},
		},
	})
	if !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Errorf("a different transfer under a split's key gave %v, want one wrapping %v", err, wallet.ErrIdempotencyConflict)
	}
}

// TestLiveRoundedEarningReadsBackWhole is D6's shape held to the
// substrate: one funding account giving, a member bucket and a rounding
// remainder receiving. It is the split the cashback domain posts most, and it is the
// one where the account travelling as the scalar end of the transaction is
// also named by every child row the ledger keeps for the legs - so a reader
// that took each row as a posting of its own would report the funding
// account as having given twice over.
func TestLiveRoundedEarningReadsBackWhole(t *testing.T) {
	t.Parallel()

	ledger := liveLedger(t)
	tag := suffix(t)
	commission := ensure(t, ledger, wallet.HouseAccount("commission-"+tag), eur)
	rounding := ensure(t, ledger, wallet.HouseAccount("rounding-"+tag), eur)
	pending := ensure(t, ledger, wallet.MemberAccount(uuid.New(), wallet.StagePending), eur)

	ref, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "earning-" + tag,
		Reference:      "entry-transition-" + tag,
		Postings: []wallet.Posting{
			{Account: commission, Amount: amount(-1000, eur)},
			{Account: pending, Amount: amount(999, eur)},
			{Account: rounding, Amount: amount(1, eur)},
		},
	})
	if err != nil {
		t.Fatalf("posting a rounded earning: %v", err)
	}

	for id, want := range map[wallet.LedgerAccountID]int64{
		commission: -1000, pending: 999, rounding: 1,
	} {
		postings := readHistory(t, ledger, id, wallet.Window{})
		if len(postings) != 1 {
			t.Errorf("account %q has %d posting(s) from one rounded earning, want 1: %v", id, len(postings), postings)
			continue
		}
		if postings[0].Amount.Minor != want {
			t.Errorf("account %q recorded %d, want %d", id, postings[0].Amount.Minor, want)
		}
		if postings[0].TransferRef != ref {
			t.Errorf("account %q recorded reference %q, want %q", id, postings[0].TransferRef, ref)
		}
		// History and the ledger's own figure must agree to the minor
		// unit; that cross-check is what D7 rests on.
		held, err := ledger.Balance(context.Background(), id, eur)
		if err != nil {
			t.Fatalf("reading %q: %v", id, err)
		}
		if held.Minor != want {
			t.Errorf("account %q holds %s but its history sums to %d", id, held, want)
		}
	}
}

// TestLiveHistoryYieldsThePostingsTheCallerWrote holds the substrate to the
// posting granularity the shared conformance suite measures all three
// implementations by. The legs are netted per account for the wire, so a
// transfer naming one account twice reaches Blnk as a single leg - and the
// postings must still read back as the two the caller wrote.
func TestLiveHistoryYieldsThePostingsTheCallerWrote(t *testing.T) {
	t.Parallel()

	accounts := liveSetup(t)
	tag := suffix(t)

	ref, err := accounts.ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "twice-" + tag,
		Reference:      "entry-transition-" + tag,
		Postings: []wallet.Posting{
			{Account: accounts.house, Amount: amount(-100, eur)},
			{Account: accounts.house, Amount: amount(-200, eur)},
			{Account: accounts.confirmed, Amount: amount(300, eur)},
		},
	})
	if err != nil {
		t.Fatalf("posting: %v", err)
	}

	postings := readHistory(t, accounts.ledger, accounts.house, wallet.Window{})
	if len(postings) != 2 {
		t.Fatalf("the house account has %d posting(s), want the 2 the caller wrote: %v", len(postings), postings)
	}
	for i, want := range []int64{-100, -200} {
		if postings[i].Amount.Minor != want {
			t.Errorf("posting %d moves %d, want %d", i+1, postings[i].Amount.Minor, want)
		}
		if postings[i].TransferRef != ref {
			t.Errorf("posting %d carries reference %q, want %q", i+1, postings[i].TransferRef, ref)
		}
	}
}

// TestLiveMixedGiversKeepTheLedgersRefusal is the shape the whole-
// transaction overdraft used to make unsafe: a member's stage account and a
// house account giving in one transfer. The permission Blnk grants is per
// transaction, so asking for it here would exempt the member too; this
// adapter does not ask, and the ledger's own refusal must still stand.
func TestLiveMixedGiversKeepTheLedgersRefusal(t *testing.T) {
	t.Parallel()

	ledger := liveLedger(t)
	tag := suffix(t)
	memberID := uuid.New()
	commission := ensure(t, ledger, wallet.HouseAccount("commission-"+tag), eur)
	float := ensure(t, ledger, wallet.HouseAccount("float-"+tag), eur)
	confirmed := ensure(t, ledger, wallet.MemberAccount(memberID, wallet.StageConfirmed), eur)
	reserved := ensure(t, ledger, wallet.MemberAccount(memberID, wallet.StageReserved), eur)

	// The float account is funded so that it is not the one going short,
	// and the member is given a little to draw on.
	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "fund-float-" + tag,
		Postings: []wallet.Posting{
			{Account: commission, Amount: amount(-6000, eur)},
			{Account: float, Amount: amount(5000, eur)},
			{Account: confirmed, Amount: amount(1000, eur)},
		},
	}); err != nil {
		t.Fatalf("funding: %v", err)
	}

	// A member giving beside a house account that holds what it gives is
	// recordable, and the member's own limit still binds.
	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "mixed-over-" + tag,
		Postings: []wallet.Posting{
			{Account: confirmed, Amount: amount(-1001, eur)},
			{Account: float, Amount: amount(-1, eur)},
			{Account: reserved, Amount: amount(1002, eur)},
		},
	}); !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("drawing a member below nothing beside a house giver gave %v, want one wrapping %v", err, wallet.ErrInsufficientFunds)
	}

	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "mixed-ok-" + tag,
		Postings: []wallet.Posting{
			{Account: confirmed, Amount: amount(-1000, eur)},
			{Account: float, Amount: amount(-1, eur)},
			{Account: reserved, Amount: amount(1001, eur)},
		},
	}); err != nil {
		t.Fatalf("a member and a solvent house account giving together: %v", err)
	}

	// A house account that would have to go negative cannot be exempted
	// without exempting the member beside it, so the transfer is refused
	// rather than recorded with the member's protection switched off.
	_, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "mixed-unsupported-" + tag,
		Postings: []wallet.Posting{
			{Account: reserved, Amount: amount(-1, eur)},
			{Account: commission, Amount: amount(-10, eur)},
			{Account: float, Amount: amount(11, eur)},
		},
	})
	if !errors.Is(err, wallet.ErrUnsupportedTransfer) {
		t.Fatalf("a house account needing an overdraft beside a member giver gave %v, want one wrapping %v", err, wallet.ErrUnsupportedTransfer)
	}
}

// TestLiveTwoLedgersOnOneServerShareNothing is the isolation
// [blnk.WithLedgerName] claims. The endpoint that finds a balance by name
// carries no ledger, and house names come from configuration - the same
// strings in every environment - so two deployments over one Blnk server
// would resolve one another's accounts if the name did not carry the ledger
// it belongs to.
func TestLiveTwoLedgersOnOneServerShareNothing(t *testing.T) {
	t.Parallel()

	tag := suffix(t)
	here := liveLedgerNamed(t, "cashback-here-"+tag)
	there := liveLedgerNamed(t, "cashback-there-"+tag)
	memberID := uuid.New()

	for _, ref := range []wallet.AccountRef{
		wallet.HouseAccount("commission"),
		wallet.MemberAccount(memberID, wallet.StageConfirmed),
	} {
		mine := ensure(t, here, ref, eur)
		yours := ensure(t, there, ref, eur)
		if mine == yours {
			t.Fatalf("%s resolved to balance %q in both ledgers; one deployment would post the other's money", ref, mine)
		}
		if _, err := here.Balance(context.Background(), yours, eur); !errors.Is(err, wallet.ErrUnknownAccount) {
			t.Errorf("reading the neighbouring ledger's balance gave %v, want one wrapping %v", err, wallet.ErrUnknownAccount)
		}
	}
}

// TestLiveRefusesWhatTheLedgerCannotRecord holds the port's two structural
// refusals against the real server, and proves each of them recorded
// nothing.
func TestLiveRefusesWhatTheLedgerCannotRecord(t *testing.T) {
	t.Parallel()

	ledger := liveLedger(t)
	tag := suffix(t)
	memberID := uuid.New()
	houseEUR := ensure(t, ledger, wallet.HouseAccount("commission-"+tag), eur)
	memberEUR := ensure(t, ledger, wallet.MemberAccount(memberID, wallet.StagePending), eur)
	houseGBP := ensure(t, ledger, wallet.HouseAccount("commission-"+tag), gbp)
	memberGBP := ensure(t, ledger, wallet.MemberAccount(memberID, wallet.StagePending), gbp)

	_, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "cross-currency-" + tag,
		Postings: []wallet.Posting{
			{Account: houseEUR, Amount: amount(-2500, eur)},
			{Account: memberEUR, Amount: amount(2500, eur)},
			{Account: houseGBP, Amount: amount(-2000, gbp)},
			{Account: memberGBP, Amount: amount(2000, gbp)},
		},
	})
	if !errors.Is(err, wallet.ErrUnsupportedTransfer) {
		t.Fatalf("a cross-currency transfer gave %v, want one wrapping %v", err, wallet.ErrUnsupportedTransfer)
	}

	_, err = ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "unknown-account-" + tag,
		Postings: []wallet.Posting{
			{Account: houseEUR, Amount: amount(-2500, eur)},
			{Account: wallet.LedgerAccountID("bln_never-issued-" + tag), Amount: amount(2500, eur)},
		},
	})
	if !errors.Is(err, wallet.ErrUnknownAccount) {
		t.Fatalf("a posting naming an unissued account gave %v, want one wrapping %v", err, wallet.ErrUnknownAccount)
	}

	_, err = ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "wrong-currency-" + tag,
		Postings: []wallet.Posting{
			{Account: houseEUR, Amount: amount(-2500, gbp)},
			{Account: memberGBP, Amount: amount(2500, gbp)},
		},
	})
	if !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Fatalf("a posting in a currency its account does not hold gave %v, want one wrapping %v", err, money.ErrCurrencyMismatch)
	}

	for _, id := range []wallet.LedgerAccountID{houseEUR, memberEUR, houseGBP, memberGBP} {
		currency := eur
		if id == houseGBP || id == memberGBP {
			currency = gbp
		}
		held, err := ledger.Balance(context.Background(), id, currency)
		if err != nil {
			t.Fatalf("reading %q: %v", id, err)
		}
		if !held.IsZero() {
			t.Errorf("account %q holds %s after three refused transfers; a refusal must change no balance", id, held)
		}
	}
}

// TestLiveBalanceRefusesWhatItShould holds the read side's refusals against
// the real server.
func TestLiveBalanceRefusesWhatItShould(t *testing.T) {
	t.Parallel()

	ledger := liveLedger(t)
	holder := ensure(t, ledger, wallet.MemberAccount(uuid.New(), wallet.StageConfirmed), eur)

	if _, err := ledger.Balance(context.Background(), holder, gbp); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("asking for the wrong currency gave %v, want one wrapping %v", err, money.ErrCurrencyMismatch)
	}
	if _, err := ledger.Balance(context.Background(), wallet.LedgerAccountID("bln_never-issued-"+suffix(t)), eur); !errors.Is(err, wallet.ErrUnknownAccount) {
		t.Errorf("asking about an account nobody issued gave %v, want one wrapping %v", err, wallet.ErrUnknownAccount)
	}
	if _, err := ledger.History(context.Background(), wallet.LedgerAccountID("bln_never-issued-"+suffix(t)), wallet.Window{}); !errors.Is(err, wallet.ErrUnknownAccount) {
		t.Errorf("History over an account nobody issued gave %v, want one wrapping %v", err, wallet.ErrUnknownAccount)
	}
}

// TestLiveHistoryWindowsPartitionTheRecord holds the window's half-open
// bounds against the real server: adjacent windows partition a history with
// no posting counted twice and none falling in a seam, which is what makes
// a watermark resume safe.
func TestLiveHistoryWindowsPartitionTheRecord(t *testing.T) {
	t.Parallel()

	accounts := liveSetup(t)
	tag := suffix(t)
	for i, minor := range []int64{100, 200, 300} {
		accounts.fund(t, fmt.Sprintf("window-%s-%d", tag, i), minor)
	}

	all := readHistory(t, accounts.ledger, accounts.confirmed, wallet.Window{})
	if len(all) != 3 {
		t.Fatalf("the member's history holds %d posting(s), want 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].PostedAt.Before(all[i-1].PostedAt) {
			t.Fatalf("posting %d is older than the one before it; history must ascend", i)
		}
	}

	// Split at the last posting's instant. A reader resuming from a
	// watermark uses the inclusive lower bound, so the two halves together
	// hold everything and the overlap is only what an idempotent reader
	// absorbs.
	at := all[len(all)-1].PostedAt
	before := readHistory(t, accounts.ledger, accounts.confirmed, wallet.Window{To: at})
	after := readHistory(t, accounts.ledger, accounts.confirmed, wallet.Window{From: at})
	if len(before)+len(after) != len(all) {
		t.Fatalf("the halves hold %d and %d postings, want %d between them", len(before), len(after), len(all))
	}
	if len(after) == 0 {
		t.Fatal("the window starting at the last posting's instant is empty; the lower bound must be inclusive")
	}
}
