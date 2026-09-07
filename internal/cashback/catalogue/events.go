// The catalogue's own announcements: which route a retailer publishes
// through, and when that changes (FR-100).
//
// One retailer has at most one published route (merchant_network_one_preferred)
// and it must be usable (merchant_network_preferred_is_publishable), so a
// route pausing or leaving the network forces a hand-over, and a hand-over
// is a fact worth a record: which route lost the slot, which took it, and
// why. Recorded on the stream rather than in a log line, because the log
// is gone in a fortnight and the question "why does this retailer publish
// through Linkwise now" arrives later than that.

package catalogue

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
	// EventProducer is the producer every event from this module is
	// appended as. The domain, not the module: the stream's consumers
	// subscribe by type, and the type's first segment names the product.
	EventProducer = "cashback"
	// TypeRoutePublished announces that a retailer's published route is now
	// the one named: on a hand-over from a route that paused or left, or
	// on a retailer that had none regaining one.
	TypeRoutePublished = EventProducer + ".route.published"
	// TypeRouteUnpublished announces that a retailer's published route was
	// withdrawn and no publishable route could take its place. The
	// retailer publishes nothing until one appears or an operator acts.
	TypeRouteUnpublished = EventProducer + ".route.unpublished"
)

// ErrNotAnnounced reports an event this module could not append. It is a
// failure of the write, never of the fact; the caller's transaction should
// not commit without it.
var ErrNotAnnounced = errors.New("catalogue: the event could not be announced")

// RouteChange is one movement of a retailer's published slot.
type RouteChange struct {
	// Merchant is the retailer whose published route moved.
	Merchant uuid.UUID
	// Route is the route that now holds the slot, or uuid.Nil when none
	// could (TypeRouteUnpublished).
	Route uuid.UUID
	// Network is the network Route belongs to; empty when Route is nil.
	Network string
	// Previous is the route that held the slot before, or uuid.Nil when
	// the retailer had none published.
	Previous uuid.UUID
	// Reason is why the slot moved, in words an operator reads.
	Reason string
	// At is the instant the run that moved it started.
	At time.Time
}

// Announcer appends this module's events.
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

// routePayload is the wire shape shared by both route events; the fields a
// case has nothing for are null rather than absent, so a consumer reads one
// schema.
type routePayload struct {
	MerchantID      string  `json:"merchant_id"`
	RouteID         *string `json:"route_id"`
	NetworkID       *string `json:"network_id"`
	PreviousRouteID *string `json:"previous_route_id"`
	Reason          string  `json:"reason"`
	At              string  `json:"at"`
}

// RoutePublished announces the route a retailer now publishes through.
func (a *Announcer) RoutePublished(ctx context.Context, db events.RowQuerier, change RouteChange) error {
	if change.Route == uuid.Nil {
		return fmt.Errorf("%w: %s names no route", ErrNotAnnounced, TypeRoutePublished)
	}
	return a.append(ctx, db, TypeRoutePublished, change)
}

// RouteUnpublished announces that a retailer's published route was
// withdrawn and nothing could take its place.
func (a *Announcer) RouteUnpublished(ctx context.Context, db events.RowQuerier, change RouteChange) error {
	if change.Previous == uuid.Nil {
		return fmt.Errorf("%w: %s names no route that was withdrawn", ErrNotAnnounced, TypeRouteUnpublished)
	}
	change.Route, change.Network = uuid.Nil, ""
	return a.append(ctx, db, TypeRouteUnpublished, change)
}

// append writes one route event. Keyed on the retailer and the run's start
// instant: one run moves a retailer's slot at most once, and a second append
// under that key is a defect in the caller rather than a state to recover
// from.
func (a *Announcer) append(ctx context.Context, db events.RowQuerier, eventType string, change RouteChange) error {
	if change.Merchant == uuid.Nil {
		return fmt.Errorf("%w: %s names no retailer", ErrNotAnnounced, eventType)
	}
	at := change.At.UTC()
	payload, err := json.Marshal(routePayload{
		MerchantID:      change.Merchant.String(),
		RouteID:         nilIfNil(change.Route),
		NetworkID:       nilIfEmpty(change.Network),
		PreviousRouteID: nilIfNil(change.Previous),
		Reason:          change.Reason,
		At:              at.Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("%w: %s about %s: %w", ErrNotAnnounced, eventType, change.Merchant, err)
	}
	if _, err := a.writer.Append(ctx, db, events.Message{
		Type:           eventType,
		Subject:        change.Merchant,
		IdempotencyKey: eventType + ":" + change.Merchant.String() + ":" + at.Format(time.RFC3339Nano),
		Payload:        payload,
	}); err != nil {
		return fmt.Errorf("%w: %s about %s: %w", ErrNotAnnounced, eventType, change.Merchant, err)
	}
	return nil
}

func nilIfNil(id uuid.UUID) *string {
	if id == uuid.Nil {
		return nil
	}
	s := id.String()
	return &s
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
