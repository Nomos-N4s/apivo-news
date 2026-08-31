package wallet_test

// What happens when the database does not answer (T080).
//
// The only cases in this file that need a fake. Every verdict a member can
// provoke - joined, already in, never joined, already gone - is one the real
// database reaches, and participations_test.go exercises those there. A
// database that FAILS is the one state no real one enters on demand, and
// these pin two things about it: the service reports it as its own error
// rather than passing a driver's up, and the handler puts it in the log and
// not on the wire.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
)

// errBroken is what a database that is not answering looks like from here.
var errBroken = errors.New("the database is not answering")

// brokenDB refuses to open a transaction, which is where both writes start.
type brokenDB struct{}

func (brokenDB) Begin(context.Context) (pgx.Tx, error) { return nil, errBroken }

// brokenEnrolments fails the read, or answers a row that is not a
// participation - the two ways this module can be handed something it
// cannot make a Participation out of.
type brokenEnrolments struct {
	row store.CashbackParticipation
	err error
}

func (b brokenEnrolments) ParticipationFor(context.Context, pgtype.UUID) (store.CashbackParticipation, error) {
	return b.row, b.err
}

// brokenService is the service over a database that answers nothing.
func brokenService(t *testing.T, enrolments wallet.Enrolments) *wallet.Participations {
	t.Helper()
	service, err := wallet.NewParticipations(brokenDB{}, enrolments, theTerms)
	if err != nil {
		t.Fatalf("NewParticipations(): %v", err)
	}
	return service
}

func TestAFailedReadIsReportedAsThisModulesOwnError(t *testing.T) {
	t.Parallel()
	service := brokenService(t, brokenEnrolments{err: errBroken})

	_, err := service.Of(context.Background(), uuid.New())
	if !errors.Is(err, wallet.ErrNotJoinable) {
		t.Fatalf("Of() returned %v, want wallet.ErrNotJoinable", err)
	}
	// Wrapped, not swallowed: whoever is paged needs the cause.
	if !errors.Is(err, errBroken) {
		t.Errorf("Of() lost the cause: %v", err)
	}
}

// A row that is not a participation must not reach a member. The database
// cannot produce one - the check constraints refuse a status nobody defined
// and a currency that is not three capitals - so this is the only place the
// refusal can be exercised, and it is worth exercising: a replica lagging a
// migration, or a hand-edited row, is what it exists for.
func TestARowThatIsNotAParticipationIsRefused(t *testing.T) {
	t.Parallel()
	sound := store.CashbackParticipation{
		AccountID:       pgtype.UUID{Bytes: uuid.New(), Valid: true},
		BrandID:         "a-brand",
		TermsVersion:    "3.1.0",
		Status:          wallet.StatusActive,
		DefaultCurrency: "SEK",
	}
	for name, breaks := range map[string]func(*store.CashbackParticipation){
		"a currency in the wrong case": func(r *store.CashbackParticipation) { r.DefaultCurrency = "sek" },
		"a blank currency":             func(r *store.CashbackParticipation) { r.DefaultCurrency = "" },
		"a status nobody defined":      func(r *store.CashbackParticipation) { r.Status = "pending" },
	} {
		row := sound
		breaks(&row)
		_, err := brokenService(t, brokenEnrolments{row: row}).Of(context.Background(), uuid.New())
		if !errors.Is(err, wallet.ErrNotParticipation) {
			t.Errorf("Of() with %s returned %v, want wallet.ErrNotParticipation", name, err)
		}
	}
}

func TestAFailedTransactionIsReportedByBothWrites(t *testing.T) {
	t.Parallel()
	service := brokenService(t, brokenEnrolments{err: pgx.ErrNoRows})
	member := uuid.New()

	if _, err := service.Join(context.Background(), member, "3.1.0"); !errors.Is(err, wallet.ErrNotJoinable) {
		t.Errorf("Join() returned %v, want wallet.ErrNotJoinable", err)
	}
	if _, err := service.Leave(context.Background(), member); !errors.Is(err, wallet.ErrNotJoinable) {
		t.Errorf("Leave() returned %v, want wallet.ErrNotJoinable", err)
	}
}

// Internals are for the log, never the wire: a 500 body that named the
// driver's error would hand a member the shape of the database.
func TestAFailedDatabaseAnswers500WithNothingInIt(t *testing.T) {
	t.Parallel()
	member := uuid.New()
	handler := wallet.NewHandler(slog.New(slog.DiscardHandler), nil, nil,
		brokenService(t, brokenEnrolments{err: errBroken}),
		fakeAuth{token: "a-token", member: member})

	for _, tc := range []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPost, `{"terms_version":"3.1.0"}`},
		{http.MethodDelete, ""},
	} {
		var req *http.Request
		if tc.body == "" {
			req = httptest.NewRequest(tc.method, wallet.ParticipationPrefix, nil)
		} else {
			req = httptest.NewRequest(tc.method, wallet.ParticipationPrefix, strings.NewReader(tc.body))
		}
		req.Header.Set("Authorization", "Bearer a-token")
		rec := serveHandler(t, handler, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s answered %d, want 500: %s", tc.method, rec.Code, rec.Body)
			continue
		}
		var problem map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
			t.Errorf("%s: the 500 body is not problem+json: %v", tc.method, err)
			continue
		}
		if detail, ok := problem["detail"]; ok && detail != "" {
			t.Errorf("%s: the 500 body carries a detail %q; internals belong in the log", tc.method, detail)
		}
	}
}
