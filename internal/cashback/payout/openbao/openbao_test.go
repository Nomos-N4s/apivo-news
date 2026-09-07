package openbao_test

// The adapter against the wire it speaks, and against the one field whose
// correctness a standard can settle.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/openbao"
)

// A structurally valid IBAN that passes its own check digits, and the
// holder that must travel with it.
const (
	goodIBAN = "DE89370400440532013000"
	holder   = "A Member"
)

// sepaDetails is one well-formed set of details for the sepa rail.
func sepaDetails(t *testing.T, iban string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"iban": iban, "holder": holder})
	if err != nil {
		t.Fatalf("building details: %v", err)
	}
	return raw
}

// vaultOver builds the adapter against the fake, with a token so the fake's
// authentication check passes.
func vaultOver(t *testing.T, f *fakeVault) *openbao.Vault {
	t.Helper()
	v, err := openbao.New(f.URL(), openbao.WithToken("a-token"))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return v
}

// TestDetailsGoToTheVaultAndTheReferenceComesBack is the happy path, checked
// on both sides: what arrived at the vault, and what the database will hold.
func TestDetailsGoToTheVaultAndTheReferenceComesBack(t *testing.T) {
	t.Parallel()
	fake := newFakeVault(t)
	details := sepaDetails(t, goodIBAN)

	reference, err := vaultOver(t, fake).Store(context.Background(), payout.KindSEPA, details)
	if err != nil {
		t.Fatalf("Store(): %v", err)
	}

	written := fake.stored()
	if len(written) != 1 {
		t.Fatalf("the vault was written to %d time(s), want once", len(written))
	}
	got := written[0]
	if got.Token != "a-token" {
		t.Errorf("the write carried token %q, want the configured one", got.Token)
	}
	if got.Kind != string(payout.KindSEPA) {
		t.Errorf("the secret records kind %q, want %q", got.Kind, payout.KindSEPA)
	}
	// The details arrive whole. A rail that received an IBAN with a field
	// dropped would fail a payment for a reason nobody could see here.
	var stored map[string]string
	if err := json.Unmarshal(got.Details, &stored); err != nil {
		t.Fatalf("the stored details are not what was sent: %v", err)
	}
	if stored["iban"] != goodIBAN || stored["holder"] != holder {
		t.Errorf("the vault holds %+v, want the details as sent", stored)
	}

	// The KV v2 write path, and the reference that names it.
	if !strings.Contains(got.Path, "/data/"+openbao.DefaultPrefix+"/") {
		t.Errorf("wrote to %q, want a KV v2 path under %q", got.Path, openbao.DefaultPrefix)
	}
	if !strings.HasPrefix(reference, "openbao:") {
		t.Errorf("reference = %q, want it to name the store it points into", reference)
	}
}

// TestTheReferenceCarriesNoneOfTheDetails is the port's own requirement, and
// the one worth a test of its own: a reference derived from the details
// would put them back in the column this arrangement exists to keep them out
// of, and would tell anyone reading the table whether two members share an
// account.
func TestTheReferenceCarriesNoneOfTheDetails(t *testing.T) {
	t.Parallel()
	fake := newFakeVault(t)
	vault := vaultOver(t, fake)

	first, err := vault.Store(context.Background(), payout.KindSEPA, sepaDetails(t, goodIBAN))
	if err != nil {
		t.Fatalf("the first destination: %v", err)
	}
	second, err := vault.Store(context.Background(), payout.KindSEPA, sepaDetails(t, goodIBAN))
	if err != nil {
		t.Fatalf("the second destination: %v", err)
	}

	for _, part := range []string{goodIBAN, strings.ToLower(goodIBAN), holder, "Member"} {
		if strings.Contains(first, part) {
			t.Errorf("the reference %q carries %q from the details", first, part)
		}
	}
	// Two members banking at the same account get different references, so
	// the reference cannot be used to compare their details.
	if first == second {
		t.Error("identical details produced identical references, so the reference is derived from them")
	}
}

// TestAnIBANThatFailsItsChecksumIsRefused. The two check digits exist to
// catch a mistyped account number, and the commonest mistype - a transposed
// pair - passes every structural test there is.
func TestAnIBANThatFailsItsChecksumIsRefused(t *testing.T) {
	t.Parallel()
	fake := newFakeVault(t)

	// The good IBAN with one adjacent pair transposed: 0130 becomes 0310.
	// Structurally it is still an IBAN, which is exactly why the check
	// digits are there.
	const transposed = "DE89370400440532031000"

	for _, tc := range []struct {
		name string
		iban string
	}{
		{"a transposed pair", transposed},
		{"one digit changed", "DE89370400440532013001"},
		{"too short to be one", "DE89"},
		{"not starting with a country", "1289370400440532013000"},
		{"empty", ""},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			_, err := vaultOver(t, fake).Store(context.Background(), payout.KindSEPA, sepaDetails(t, tc.iban))
			if !errors.Is(err, payout.ErrDetailsRefused) {
				t.Fatalf("Store() = %v, want one wrapping %v", err, payout.ErrDetailsRefused)
			}
			// Refused without quoting it back: an error reaches a log, a
			// trace and a support ticket, and this one would carry a bank
			// account into all three.
			if tc.iban != "" && strings.Contains(err.Error(), tc.iban) {
				t.Errorf("the refusal %q quotes the account back", err)
			}
		})
	}

	if written := fake.stored(); len(written) != 0 {
		t.Errorf("%d refused destination(s) reached the vault", len(written))
	}
}

// TestASepaDestinationNamesItsHolder. A transfer needs a name as well as an
// account, and a bank that receives one without the other rejects it.
func TestASepaDestinationNamesItsHolder(t *testing.T) {
	t.Parallel()
	fake := newFakeVault(t)

	raw, err := json.Marshal(map[string]string{"iban": goodIBAN})
	if err != nil {
		t.Fatalf("building details: %v", err)
	}
	if _, err := vaultOver(t, fake).Store(context.Background(), payout.KindSEPA, raw); !errors.Is(err, payout.ErrDetailsRefused) {
		t.Fatalf("Store() without a holder = %v, want one wrapping %v", err, payout.ErrDetailsRefused)
	}
}

// TestTheOtherRailsAreOpenButMustSaySomething. The contract calls the
// details "deliberately unconstrained... what belongs in it differs by
// rail", so only sepa has a shape to check. Empty is still refused: a
// destination that says nowhere is one no rail could pay.
func TestTheOtherRailsAreOpenButMustSaySomething(t *testing.T) {
	t.Parallel()
	fake := newFakeVault(t)
	ctx := context.Background()

	note, err := json.Marshal(map[string]string{"note": "pay by hand, IBAN in the vault"})
	if err != nil {
		t.Fatalf("building details: %v", err)
	}
	for _, kind := range []payout.Kind{payout.KindManual, payout.KindStub} {
		if _, err := vaultOver(t, fake).Store(ctx, kind, note); err != nil {
			t.Errorf("a %s destination with a note was refused: %v", kind, err)
		}
		if _, err := vaultOver(t, fake).Store(ctx, kind, json.RawMessage(`{}`)); !errors.Is(err, payout.ErrDetailsRefused) {
			t.Errorf("an empty %s destination = %v, want one wrapping %v", kind, err, payout.ErrDetailsRefused)
		}
		if _, err := vaultOver(t, fake).Store(ctx, kind, json.RawMessage(`"a string"`)); !errors.Is(err, payout.ErrDetailsRefused) {
			t.Errorf("a %s destination whose details are not an object = %v, want one wrapping %v", kind, err, payout.ErrDetailsRefused)
		}
	}
}

// TestAVaultThatRefusesIsNotTheMembersMistake. The two failures answer
// differently at the endpoint - 400 for the member, 502 for the deployment -
// so they must be distinguishable here.
func TestAVaultThatRefusesIsNotTheMembersMistake(t *testing.T) {
	t.Parallel()
	fake := newFakeVault(t)
	fake.answers(http.StatusForbidden)

	_, err := vaultOver(t, fake).Store(context.Background(), payout.KindSEPA, sepaDetails(t, goodIBAN))
	if err == nil {
		t.Fatal("Store() succeeded although the vault refused")
	}
	if errors.Is(err, payout.ErrDetailsRefused) {
		t.Errorf("a vault failure reads as the member's mistake: %v", err)
	}
	// The vault echoes the secret's path in its errors. Forwarding that
	// body would put the location of a member's bank details into a log
	// beside the reason the write failed.
	if strings.Contains(err.Error(), openbao.DefaultPrefix) {
		t.Errorf("the error %q forwards the vault's body, which names the secret's path", err)
	}
}

// TestAnUnreachableVaultRecordsNothing. The endpoint answers 502 and no
// destination row is written, so there is no row pointing at a secret that
// does not exist.
func TestAnUnreachableVaultRecordsNothing(t *testing.T) {
	t.Parallel()

	// An address nothing listens on: New accepts it, the call fails.
	vault, err := openbao.New("http://127.0.0.1:1", openbao.WithToken("a-token"))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if _, err := vault.Store(context.Background(), payout.KindSEPA, sepaDetails(t, goodIBAN)); err == nil {
		t.Fatal("Store() succeeded against nothing")
	}
}

// TestAVaultNeedsAnEndpointItCanReach covers the construction refusals. A
// deployment that misconfigured this must be told at startup, not by the
// first member who tries to be paid.
func TestAVaultNeedsAnEndpointItCanReach(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{"", "   ", "not a url", "ftp://vault.example", "/v1/secret"} {
		if _, err := openbao.New(endpoint); err == nil {
			t.Errorf("New(%q) was accepted", endpoint)
		}
	}
	// The endpoint travels beside a token, so a value pasted into the wrong
	// key must not print itself into a startup log.
	secretish := "https://vault.example/?token=hunter2"
	if _, err := openbao.New("ftp://" + secretish); err != nil && strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the refusal %q repeats the value it refused", err)
	}
	if _, err := openbao.New("https://vault.example", openbao.WithMount(" ")); err == nil {
		t.Error("a vault with no mount was accepted")
	}
}

// TestDetailsTooLargeToBeADestinationAreRefused. A rail needs an account
// number and a name; anything at this size is a mistake or an attempt to use
// the vault as storage.
func TestDetailsTooLargeToBeADestinationAreRefused(t *testing.T) {
	t.Parallel()
	fake := newFakeVault(t)

	huge, err := json.Marshal(map[string]string{"iban": goodIBAN, "holder": strings.Repeat("a", 16<<10)})
	if err != nil {
		t.Fatalf("building details: %v", err)
	}
	if _, err := vaultOver(t, fake).Store(context.Background(), payout.KindSEPA, huge); !errors.Is(err, payout.ErrDetailsRefused) {
		t.Fatalf("Store() = %v, want one wrapping %v", err, payout.ErrDetailsRefused)
	}
	if written := fake.stored(); len(written) != 0 {
		t.Errorf("%d oversized destination(s) reached the vault", len(written))
	}
}

// TestValidIBANAcceptsRealOnes keeps the checksum honest in the other
// direction: a validator that refused everything would pass every test
// above and stop every member being paid.
func TestValidIBANAcceptsRealOnes(t *testing.T) {
	t.Parallel()

	// Published example IBANs from several SEPA countries, including the
	// two markets this deployment serves.
	for _, iban := range []string{
		"DE89370400440532013000",
		"GR1601101250000000012300695",
		"FR1420041010050500013M02606",
		"ES9121000418450200051332",
		"NL91ABNA0417164300",
		"de89 3704 0044 0532 0130 00",
	} {
		if !openbao.ValidIBAN(iban) {
			t.Errorf("ValidIBAN(%q) = false, want true", iban)
		}
	}
}
