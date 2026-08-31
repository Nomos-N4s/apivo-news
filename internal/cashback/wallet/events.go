// The two facts this module publishes: that a member joined cashback, and
// that one left it (T080, FR-002, FR-003).
//
// Both are appended to the outbox in the transaction that made the fact
// true, which is the whole of the contract's atomicity guarantee - "there
// is no code path that publishes an event without its state change, or
// commits a state change without its event". Nothing here opens that
// transaction and nothing here retries: the caller owns both.
//
// What is NOT here is the wallet's own reads. GET /wallet and
// GET /wallet/entries change nothing, and a read publishes no fact.

package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
)

const (
	// EventProducer is the domain these facts belong to, and the first
	// segment of both types below - the writer refuses a type naming
	// another producer, because a domain publishes only its own facts.
	EventProducer = "cashback"
	// TypeParticipationStarted announces one member who is now in cashback,
	// and which terms they accepted to get there.
	TypeParticipationStarted = EventProducer + ".participation.started"
	// TypeParticipationEnded announces one member who has left (FR-003).
	//
	// It carries the terms version too, and that is not redundancy: a
	// consumer holding the started and the ended events for one member has
	// the whole of what they agreed to and for how long, without reading
	// this module's tables - which is what consumer rule 2 asks of a
	// payload.
	TypeParticipationEnded = EventProducer + ".participation.ended"
)

// ErrNotAnnounced reports an event that could not be appended beside the
// participation change it describes.
//
// Always fatal to the caller that raised it. The append shares the
// transaction the change was written in, so a swallowed failure would
// either commit an opt-in with no event or - because the outbox reports a
// collision as a unique violation, which aborts the transaction - commit
// nothing at all while answering the member that they had joined.
var ErrNotAnnounced = errors.New("wallet: the event could not be appended beside the participation change")

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

// Started announces one acceptance the database has just recorded.
//
// NO IDEMPOTENCY KEY, and that is the considered choice rather than an
// omission. A member may accept more than once - leaving and coming back is
// a second acceptance of whatever terms are in force then, and a fact in
// its own right - so the key would have to be singular per ACCEPTANCE, and
// the schema holds no acceptance identity to build one from: participation
// is keyed on the account, and each re-join overwrites the row. Every
// candidate is therefore wrong in a way that costs a member something.
// The account alone silences every re-join after the first. The account and
// the terms version silences a re-join under unchanged terms, which is the
// commonest re-join there is. The account and opted_in_at looks right and
// is the worst of the three: timestamps are microseconds in Postgres, so
// two acceptances recorded in one microsecond collide - and a collision is
// a unique violation, which aborts the transaction, so a member's legitimate
// re-join would fail with a 500 rather than being deduplicated.
//
// What the key would have protected is already protected, and better: the
// upsert refuses an active member outright, so a retried POST never reaches
// this at all, and a retry after a failed commit re-runs the upsert against
// a row the rollback restored. The ROW is the uniqueness constraint here;
// a second one in the outbox could only disagree with it.
//
// The instant is the row's, read back rather than taken from a clock, so
// the event and the record name one moment.
func (a *Announcer) Started(ctx context.Context, db events.RowQuerier, joined Participation) error {
	if joined.Member == uuid.Nil {
		return fmt.Errorf("%w: %s about a member the database did not record", ErrNotAnnounced, TypeParticipationStarted)
	}
	payload, err := json.Marshal(struct {
		AccountID    uuid.UUID `json:"account_id"`
		TermsVersion string    `json:"terms_version"`
		At           time.Time `json:"at"`
	}{
		AccountID:    joined.Member,
		TermsVersion: joined.TermsVersion,
		At:           joined.OptedInAt,
	})
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotAnnounced, TypeParticipationStarted, err)
	}
	return a.append(ctx, db, TypeParticipationStarted, joined.Member, payload)
}

// Ended announces one member the database has just closed.
//
// Unkeyed, for the reason [Announcer.Started] is: a member who re-joins can
// leave again, the second departure is a second fact, and the schema holds
// no departure identity to make one singular by. The guard is the statement
// instead - it narrows on status = 'active', so it closes a participation
// exactly once and a repeated DELETE never reaches here.
func (a *Announcer) Ended(ctx context.Context, db events.RowQuerier, left Participation) error {
	switch {
	case left.Member == uuid.Nil:
		return fmt.Errorf("%w: %s about a member the database did not record", ErrNotAnnounced, TypeParticipationEnded)
	case left.LeftAt.IsZero():
		// The schema makes "left, but we do not know when" unrepresentable
		// (participation_left_has_timestamp), so a departure with no date is
		// a row the database would have refused - and announcing it would
		// put a fact in the stream that is not in the schema.
		return fmt.Errorf("%w: %s about a departure with no date", ErrNotAnnounced, TypeParticipationEnded)
	}
	payload, err := json.Marshal(struct {
		AccountID    uuid.UUID `json:"account_id"`
		TermsVersion string    `json:"terms_version"`
		At           time.Time `json:"at"`
	}{
		AccountID:    left.Member,
		TermsVersion: left.TermsVersion,
		At:           left.LeftAt,
	})
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotAnnounced, TypeParticipationEnded, err)
	}
	return a.append(ctx, db, TypeParticipationEnded, left.Member, payload)
}

// append writes one message and wraps whatever comes back, so every caller
// reports the same error and names the same subject.
func (a *Announcer) append(ctx context.Context, db events.RowQuerier, eventType string, subject uuid.UUID, payload json.RawMessage) error {
	if _, err := a.writer.Append(ctx, db, events.Message{
		Type:    eventType,
		Subject: subject,
		Payload: payload,
	}); err != nil {
		return fmt.Errorf("%w: %s about member %s: %w", ErrNotAnnounced, eventType, subject, err)
	}
	return nil
}
