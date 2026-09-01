// The two facts this module publishes: that an entry came into being, and
// that one of them moved (T076).
//
// Both are appended to the outbox in the transaction that made the fact
// true. That placement is the whole of the contract's atomicity guarantee -
// "there is no code path that publishes an event without its state change,
// or commits a state change without its event" - and it is why nothing here
// opens a transaction and nothing here retries.
//
// What is NOT here is as deliberate. cashback.entry.reversed is a third
// fact about a pair of entries rather than about either one, and it is not
// this task's. And cashback.click.created belongs beside the insert that
// records the click, which is in clickout: an event announced from here
// would be announced by whatever transaction happened to be open, which is
// no transaction at all on the click path.

package earnings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

const (
	// EventProducer is the domain these facts belong to, and the first
	// segment of every type below - the writer refuses a type naming another
	// producer, because a domain publishes only its own facts.
	EventProducer = "cashback"
	// TypeEntryCreated announces one entry that now exists: who is owed, how
	// much, and the state it was born in.
	TypeEntryCreated = EventProducer + ".entry.created"
	// TypeEntryStateChanged announces one entry that moved, and names the
	// ledger transfer that made the move real. A consumer holding both can
	// follow the money without ever reading this module's tables, which is
	// what consumer rule 2 asks of the payload.
	TypeEntryStateChanged = EventProducer + ".entry.state_changed"
	// TypeTransactionUnattributed announces a report nobody can be credited
	// for (FR-034).
	//
	// The SAME type the networks module publishes, and deliberately: FR-034's
	// queue has two feeders - a report that carried no reference at all, and
	// one whose reference matched no click - and they are the same fact to
	// everyone downstream. Two types would make an operator's queue something
	// a consumer had to assemble from two streams and know to. The
	// idempotency key is the type and the report, so the two feeders cannot
	// announce one report twice however they race; they never see the same
	// report anyway, because a report either named a reference or did not.
	TypeTransactionUnattributed = EventProducer + ".transaction.unattributed"
)

// ErrNotAnnounced reports an event that could not be appended beside the
// state change it describes.
//
// It is always fatal to the caller that raised it. The append shares the
// caller's transaction, so a swallowed failure would commit a moved entry
// with no event - the one thing the contract says no code path may do - and
// nothing would come back to notice.
var ErrNotAnnounced = errors.New("earnings: the event could not be appended beside the state change")

// Announcer appends this module's facts to the outbox.
//
// It holds no database handle. Every method takes the transaction that
// carries the state change, because the placement of the append - not
// anything this type does - is what makes the event and the fact atomic.
type Announcer struct {
	writer *events.Writer
}

// NewAnnouncer builds the announcer for this domain.
func NewAnnouncer() (*Announcer, error) {
	writer, err := events.NewWriter(EventProducer)
	if err != nil {
		return nil, err
	}
	return &Announcer{writer: writer}, nil
}

// Created announces one entry the database has just inserted.
//
// The amount travels in the payload rather than being left to a lookup, and
// it travels as {minor, currency} - the shape C-6 mandates everywhere else,
// for the reason it mandates it everywhere else: a consumer that received a
// bare integer would have to decide a currency, and the one it decided
// would be right until it was not.
//
// Keyed on the entry, because an entry is created once. A second append
// under that key means two creations of one entry in one transaction, which
// is a defect in the caller rather than a state to recover from.
func (a *Announcer) Created(ctx context.Context, db events.RowQuerier, entry Entry) error {
	if entry.ID == uuid.Nil {
		return fmt.Errorf("%w: %s about an entry the database did not insert", ErrNotAnnounced, TypeEntryCreated)
	}
	payload, err := json.Marshal(struct {
		EntryID   uuid.UUID    `json:"entry_id"`
		AccountID uuid.UUID    `json:"account_id"`
		State     State        `json:"state"`
		Amount    money.Amount `json:"amount"`
		At        time.Time    `json:"at"`
	}{
		EntryID:   entry.ID,
		AccountID: entry.Member,
		State:     entry.State,
		Amount:    entry.Amount,
		// The insertion instant the ROW carries, read back rather than taken
		// from the clock, so the event and the entry name one moment. A
		// consumer joining the two on time would otherwise find them
		// disagreeing by however long the transaction stayed open.
		At: entry.CreatedAt,
	})
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotAnnounced, TypeEntryCreated, err)
	}
	return a.append(ctx, db, TypeEntryCreated, entry.ID, TypeEntryCreated+":"+entry.ID.String(), payload)
}

// StateChanged announces one move the database has just recorded.
//
// Keyed on the ledger transfer AND the entry, because a transfer does not
// always carry one entry. A transition posted by [Entries.Apply] has a
// transfer to itself - the key is derived from the entry, the cause and the
// destination state (D8), so a retry of the same move re-posts to the SAME
// transfer - and for those two the transfer alone would be enough. A
// reservation is the exception that decides this: [Entries.Reserve] posts ONE
// transfer for every entry a withdrawal claims, because C-7 finds them by
// matching that single reference (migration 0016). Keyed on the transfer
// alone, the first entry of a five-entry reservation would be announced and
// the other four would collide on an already-appended key - which the outbox
// reports as a unique violation that aborts the whole transaction, so the
// reservation would not be half-announced, it would not commit at all.
//
// Adding the entry refuses exactly the announcement a retry would duplicate
// and refuses nothing else: a retry re-posts to the same transfer and moves
// the same entries, so it re-derives the same keys.
//
// The subject is the entry, not the transfer, because per-subject ordering
// is the only ordering the stream guarantees and the order that matters here
// is the order one entry moved in.
func (a *Announcer) StateChanged(ctx context.Context, db events.RowQuerier, moved Transition) error {
	switch {
	case moved.Entry == uuid.Nil:
		return fmt.Errorf("%w: %s about an entry the database did not move", ErrNotAnnounced, TypeEntryStateChanged)
	case moved.Transfer == "":
		// D7: no state is ever recorded without its posting. A move naming
		// no transfer is a move the database would have refused, so
		// announcing it would put a fact in the stream that is not in the
		// schema.
		return fmt.Errorf("%w: %s about a move naming no transfer", ErrNotAnnounced, TypeEntryStateChanged)
	}
	payload, err := json.Marshal(struct {
		EntryID uuid.UUID `json:"entry_id"`
		// From is empty on an opening transition, which is what the schema
		// stores: an entry that came into being did not come from anywhere.
		From              State     `json:"from"`
		To                State     `json:"to"`
		LedgerTransferRef string    `json:"ledger_transfer_ref"`
		At                time.Time `json:"at"`
	}{
		EntryID:           moved.Entry,
		From:              moved.From,
		To:                moved.To,
		LedgerTransferRef: string(moved.Transfer),
		// The instant the transition row carries, for the same reason
		// [Announcer.Created] reads the entry's.
		At: moved.At,
	})
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotAnnounced, TypeEntryStateChanged, err)
	}
	return a.append(ctx, db, TypeEntryStateChanged, moved.Entry,
		TypeEntryStateChanged+":"+string(moved.Transfer)+":"+moved.Entry.String(), payload)
}

// Unattributed announces one report whose reference matched no click.
//
// It says only which report and when it was noticed, which is the whole of
// the contract's payload for this type. What an operator needs in order to
// act - the money, the network's own identifier, whether it may be
// attributed at all - is read from the queue, not carried here: an event
// that carried it would be a second copy of facts that can change
// underneath it.
//
// Announced per ROW WRITTEN, never per miss observed. A window re-read after
// a crash resolves the same references again and writes nothing, and a
// consumer must not see the same report queued four times a day for it.
func (a *Announcer) Unattributed(ctx context.Context, db events.RowQuerier, queued Unmatched) error {
	if queued.ReportID == uuid.Nil {
		return fmt.Errorf("%w: %s about a report the database did not queue", ErrNotAnnounced, TypeTransactionUnattributed)
	}
	payload, err := json.Marshal(struct {
		NetworkTransactionID uuid.UUID `json:"network_transaction_id"`
		At                   time.Time `json:"at"`
	}{
		NetworkTransactionID: queued.ReportID,
		// The detection instant the queue ROW carries, for the reason every
		// other instant here is read back rather than taken from a clock.
		At: queued.DetectedAt,
	})
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotAnnounced, TypeTransactionUnattributed, err)
	}
	return a.append(ctx, db, TypeTransactionUnattributed, queued.ReportID,
		TypeTransactionUnattributed+":"+queued.ReportID.String(), payload)
}

// append writes one event under the given key.
//
// An already-appended key is a FAILURE here, not a no-op, and the reason is
// worth stating because the opposite is the tempting reading. The outbox
// reports a collision as a unique violation, and a failed statement aborts
// the whole transaction - so a caller that shrugged it off would carry on
// through a transaction whose every later statement raises "current
// transaction is aborted", and would commit nothing while reporting success.
func (a *Announcer) append(ctx context.Context, db events.RowQuerier, eventType string, subject uuid.UUID, key string, payload json.RawMessage) error {
	if _, err := a.writer.Append(ctx, db, events.Message{
		Type:           eventType,
		Subject:        subject,
		IdempotencyKey: key,
		Payload:        payload,
	}); err != nil {
		return fmt.Errorf("%w: %s about %s: %w", ErrNotAnnounced, eventType, subject, err)
	}
	return nil
}
