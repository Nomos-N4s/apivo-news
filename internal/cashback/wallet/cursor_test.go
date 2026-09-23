package wallet

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestEncodeAndDecodeCursor_RoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.March, 1, 14, 30, 45, 123456000, time.FixedZone("EST", -5*3600))
	id := uuid.New()

	encoded := encodeCursor(now, id)
	if encoded == "" {
		t.Fatal("encodeCursor returned empty string")
	}

	gotAt, gotID, err := decodeCursor(encoded)
	if err != nil {
		t.Fatalf("decodeCursor(%q) unexpected error: %v", encoded, err)
	}

	if !gotAt.Valid {
		t.Error("decodeCursor got invalid Timestamptz")
	}
	if !gotAt.Time.Equal(now.UTC()) {
		t.Errorf("decodeCursor timestamp = %v, want %v", gotAt.Time, now.UTC())
	}
	if gotAt.Time.Location() != time.UTC {
		t.Errorf("decodeCursor timestamp location = %v, want UTC", gotAt.Time.Location())
	}

	if !gotID.Valid {
		t.Error("decodeCursor got invalid UUID")
	}
	if uuid.UUID(gotID.Bytes) != id {
		t.Errorf("decodeCursor rowID = %v, want %v", uuid.UUID(gotID.Bytes), id)
	}
}

func TestDecodeCursor_Cases(t *testing.T) {
	t.Parallel()

	validID := uuid.New()
	validTimeStr := "2026-03-01T12:00:00.123456Z"

	encodeRaw := func(raw string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(raw))
	}

	tests := []struct {
		name    string
		cursor  string
		wantErr error
		check   func(t *testing.T, at pgtype.Timestamptz, id pgtype.UUID)
	}{
		{
			name:   "empty cursor (first page)",
			cursor: "",
			check: func(t *testing.T, at pgtype.Timestamptz, id pgtype.UUID) {
				if at.Valid || id.Valid {
					t.Errorf("expected zero values for empty cursor, got at.Valid=%v, id.Valid=%v", at.Valid, id.Valid)
				}
			},
		},
		{
			name:    "exceeds maxCursorBytes",
			cursor:  strings.Repeat("a", maxCursorBytes+1),
			wantErr: ErrBadCursor,
		},
		{
			name:    "invalid base64 encoding",
			cursor:  "!!!invalid-base64-url!!!",
			wantErr: ErrBadCursor,
		},
		{
			name:    "wrong tag",
			cursor:  encodeRaw("other-tag|" + validTimeStr + "|" + validID.String()),
			wantErr: ErrBadCursor,
		},
		{
			name:    "missing timestamp separator",
			cursor:  encodeRaw("wallet-entries"),
			wantErr: ErrBadCursor,
		},
		{
			name:    "missing uuid separator",
			cursor:  encodeRaw("wallet-entries|" + validTimeStr),
			wantErr: ErrBadCursor,
		},
		{
			name:    "invalid timestamp format",
			cursor:  encodeRaw("wallet-entries|not-a-timestamp|" + validID.String()),
			wantErr: ErrBadCursor,
		},
		{
			name:    "invalid uuid format",
			cursor:  encodeRaw("wallet-entries|" + validTimeStr + "|not-a-uuid"),
			wantErr: ErrBadCursor,
		},
		{
			name:   "valid encoded cursor",
			cursor: encodeRaw("wallet-entries|" + validTimeStr + "|" + validID.String()),
			check: func(t *testing.T, at pgtype.Timestamptz, id pgtype.UUID) {
				if !at.Valid || !id.Valid {
					t.Fatalf("expected valid cursor result, got at.Valid=%v, id.Valid=%v", at.Valid, id.Valid)
				}
				if uuid.UUID(id.Bytes) != validID {
					t.Errorf("got uuid %v, want %v", uuid.UUID(id.Bytes), validID)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			at, id, err := decodeCursor(tt.cursor)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("decodeCursor(%q) error = %v, want %v", tt.cursor, err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("decodeCursor(%q) unexpected error = %v", tt.cursor, err)
			}

			if tt.check != nil {
				tt.check(t, at, id)
			}
		})
	}
}

func TestEncodeCursor_Structure(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.January, 15, 10, 20, 30, 999000, time.UTC)
	id := uuid.New()

	encoded := encodeCursor(at, id)
	decodedBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("failed to decode base64url cursor: %v", err)
	}

	parts := strings.Split(string(decodedBytes), "|")
	if len(parts) != 3 {
		t.Fatalf("decoded payload split count = %d, want 3 (got: %q)", len(parts), string(decodedBytes))
	}

	if parts[0] != cursorTag {
		t.Errorf("tag = %q, want %q", parts[0], cursorTag)
	}
	if parts[1] != at.Format(time.RFC3339Nano) {
		t.Errorf("timestamp = %q, want %q", parts[1], at.Format(time.RFC3339Nano))
	}
	if parts[2] != id.String() {
		t.Errorf("id = %q, want %q", parts[2], id.String())
	}
}
