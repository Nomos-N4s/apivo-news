package ops

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// cursorSeparator joins the parts of an encoded cursor. None of them can
// contain it: one is a list tag from the constants below, one an RFC 3339
// timestamp, one a uuid.
const cursorSeparator = "|"

// The list a cursor belongs to. Every operator queue pages on a
// (timestamp, id) pair, so without this tag one queue's cursor would decode
// cleanly as a position in another - and a client replaying one against the
// wrong queue would get a 200 with rows silently missing instead of the 400
// the contract promises for a cursor the endpoint did not issue.
const (
	unattributedCursors = "unattributed"
	differenceCursors   = "differences"
)

// maxCursorBytes bounds the encoded cursor the endpoints will even look at,
// mirroring the other modules: a real one is a short tag, an RFC 3339
// timestamp and a UUID - comfortably under a hundred bytes - so anything
// larger is malformed by construction and is rejected before it is decoded.
// The shared Cursor parameter in api/openapi.json documents exactly this
// bound.
const maxCursorBytes = 256

// errBadCursor reports a cursor that did not come from encodeCursor - or
// came from a different list's encodeCursor, or was corrupted on the way
// back. The endpoint maps it to 400 rather than silently restarting from
// the first page: a paging client that gets page one back when it asked for
// page four would loop forever, and on a queue that decides money it would
// loop forever over work it had already judged.
var errBadCursor = errors.New("ops: malformed cursor")

// encodeCursor renders a keyset position - a timestamp and the row id that
// tie-breaks it - as the contract's opaque cursor, tagged with the list that
// issued it.
//
// The encoding is deliberately not part of the contract: it is base64url of
// "<list>|<RFC 3339 UTC timestamp>|<uuid>", and clients must only ever echo
// back what next_cursor gave them.
//
// The timestamp is normalised to UTC so the same row yields the same cursor
// whatever the server's local zone, and rendered with sub-second precision
// so a Postgres microsecond timestamp survives the round trip - truncating
// it would move the keyset boundary and silently skip or repeat rows.
func encodeCursor(list string, at time.Time, rowID uuid.UUID) string {
	raw := list + cursorSeparator + at.UTC().Format(time.RFC3339Nano) + cursorSeparator + rowID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses a cursor produced by encodeCursor for the given list,
// reporting errBadCursor for anything else. Requiring the tag is what makes
// "a cursor this endpoint issued" checkable rather than assumed.
func decodeCursor(list, raw string) (at time.Time, rowID uuid.UUID, err error) {
	if len(raw) > maxCursorBytes {
		return time.Time{}, uuid.UUID{}, errBadCursor
	}
	blob, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, uuid.UUID{}, errBadCursor
	}
	tag, rest, ok := strings.Cut(string(blob), cursorSeparator)
	if !ok || tag != list {
		return time.Time{}, uuid.UUID{}, errBadCursor
	}
	stamp, id, ok := strings.Cut(rest, cursorSeparator)
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
