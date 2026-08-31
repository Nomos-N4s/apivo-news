// Where a page of history left off (T079).
//
// Keyset, never an offset. A member's entries are inserted while they page -
// a poll runs, a network confirms - and an offset would show them a row
// twice or skip one entirely. The position is the last row's own
// (created_at, id), so the next page starts exactly after it however many
// rows arrived in between.

package wallet

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5/pgtype"
)

// cursorSeparator joins the parts of an encoded cursor. Neither can contain
// it: one is an RFC 3339 timestamp, one a uuid.
const cursorSeparator = "|"

// cursorTag marks a cursor as this list's.
//
// One list here today, and the tag exists anyway: the wallet will page
// payouts and withdrawals before long, and a cursor from one decoding
// cleanly as a position in another would answer 200 with rows silently
// missing rather than the 400 the contract promises for a cursor this
// endpoint did not issue.
const cursorTag = "wallet-entries"

// maxCursorBytes bounds the encoded cursor this module will even look at,
// mirroring the operator surface: a real one is a short tag, an RFC 3339
// timestamp and a uuid, comfortably under a hundred bytes, so anything
// larger is malformed by construction and is refused before it is decoded.
const maxCursorBytes = 256

// encodeCursor renders a keyset position as the contract's opaque cursor.
//
// The encoding is deliberately not part of the contract: it is base64url of
// "<tag>|<RFC 3339 UTC timestamp>|<uuid>", and clients must only ever echo
// back what next_cursor gave them.
//
// The timestamp is normalised to UTC so one row yields one cursor whatever
// the server's zone, and rendered with sub-second precision so a Postgres
// microsecond timestamp survives the round trip - truncating it would move
// the boundary and silently skip or repeat rows.
func encodeCursor(at time.Time, rowID uuid.UUID) string {
	raw := cursorTag + cursorSeparator + at.UTC().Format(time.RFC3339Nano) + cursorSeparator + rowID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses a cursor produced by encodeCursor, reporting
// ErrBadCursor for anything else. An empty cursor is the first page and is
// not an error: it is what a client that has never paged sends.
func decodeCursor(raw string) (pgtype.Timestamptz, pgtype.UUID, error) {
	if raw == "" {
		return pgtype.Timestamptz{}, pgtype.UUID{}, nil
	}
	if len(raw) > maxCursorBytes {
		return pgtype.Timestamptz{}, pgtype.UUID{}, ErrBadCursor
	}
	blob, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, ErrBadCursor
	}
	tag, rest, ok := strings.Cut(string(blob), cursorSeparator)
	if !ok || tag != cursorTag {
		return pgtype.Timestamptz{}, pgtype.UUID{}, ErrBadCursor
	}
	stamp, id, ok := strings.Cut(rest, cursorSeparator)
	if !ok {
		return pgtype.Timestamptz{}, pgtype.UUID{}, ErrBadCursor
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, ErrBadCursor
	}
	rowID, err := uuid.Parse(id)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, ErrBadCursor
	}
	return pgtype.Timestamptz{Time: at, Valid: true}, pgtype.UUID{Bytes: rowID, Valid: true}, nil
}
