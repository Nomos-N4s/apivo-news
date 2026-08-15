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

// encodeCursor renders a keyset position as the contract's opaque cursor.
// The encoding is deliberately not part of the contract: it is base64url of
// "<RFC 3339 UTC timestamp>|<uuid>", and clients must only ever echo back
// what next_cursor gave them.
//
// The timestamp is normalised to UTC so the same row yields the same cursor
// whatever the server's local zone, and rendered with sub-second precision
// so a Postgres microsecond timestamp survives the round trip - truncating
// it would move the keyset boundary and silently skip or repeat rows.
func encodeCursor(c QueueCursor) string {
	raw := c.RetrievedAt.UTC().Format(time.RFC3339Nano) + cursorSeparator + c.RowID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses a cursor produced by encodeCursor, reporting
// errBadCursor for anything else.
func decodeCursor(raw string) (QueueCursor, error) {
	blob, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return QueueCursor{}, errBadCursor
	}
	at, id, ok := strings.Cut(string(blob), cursorSeparator)
	if !ok {
		return QueueCursor{}, errBadCursor
	}
	retrievedAt, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return QueueCursor{}, errBadCursor
	}
	rowID, err := uuid.Parse(id)
	if err != nil {
		return QueueCursor{}, errBadCursor
	}
	return QueueCursor{RetrievedAt: retrievedAt, RowID: rowID}, nil
}
