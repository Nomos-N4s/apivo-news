package editorial

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// cursorSeparator joins the two halves of a keyset position. Neither half
// can contain it: one is an RFC 3339 timestamp, the other a uuid.
const cursorSeparator = "|"

// errBadCursor reports a cursor that did not come from encodeCursor, or was
// corrupted on the way back. The endpoint maps it to 400 rather than
// silently restarting from the first page - a paging client that gets page
// one back when it asked for page four would loop forever.
var errBadCursor = errors.New("editorial: malformed cursor")

// encodeCursor renders a keyset position - a timestamp and the row id that
// tie-breaks it - as the contract's opaque cursor. Every keyset list in
// this module pages on that same pair (the queue on retrieved_at, the
// sources on created_at), so this one codec serves them all rather than
// each endpoint forking its own.
//
// The encoding is deliberately not part of the contract: it is base64url of
// "<RFC 3339 UTC timestamp>|<uuid>", and clients must only ever echo back
// what next_cursor gave them.
//
// The timestamp is normalised to UTC so the same row yields the same cursor
// whatever the server's local zone, and rendered with sub-second precision
// so a Postgres microsecond timestamp survives the round trip - truncating
// it would move the keyset boundary and silently skip or repeat rows.
func encodeCursor(at time.Time, rowID uuid.UUID) string {
	raw := at.UTC().Format(time.RFC3339Nano) + cursorSeparator + rowID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses a cursor produced by encodeCursor, reporting
// errBadCursor for anything else. Which list's position the pair marks is
// the endpoint's business; the codec only carries it.
func decodeCursor(raw string) (at time.Time, rowID uuid.UUID, err error) {
	blob, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, uuid.UUID{}, errBadCursor
	}
	stamp, id, ok := strings.Cut(string(blob), cursorSeparator)
	if !ok {
		return time.Time{}, uuid.UUID{}, errBadCursor
	}
	at, err = time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, uuid.UUID{}, errBadCursor
	}
	rowID, err = uuid.Parse(id)
	if err != nil {
		return time.Time{}, uuid.UUID{}, errBadCursor
	}
	return at, rowID, nil
}
