// The tests for reported.go: one broken rule at a time against a well-formed
// transaction and a well-formed catalogue entry. It holds portTestReport and
// portTestMerchant - and the click reference they are built from - because
// this is the lowest-dependency file that needs them.

package networks_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// Two currencies are enough to prove every per-currency rule, and they live
// here rather than in the package because which currencies a deployment
// trades in is configuration, not a property of the port.
const (
	portTestEUR = money.Currency("EUR")
	portTestGBP = money.Currency("GBP")
)

// portTestRefValue is a reference of exactly the length the click table
// requires - 22 URL-safe characters, the first length at or above the 128
// bits FR-020 specifies - so a case that shortens it by one proves the bound
// rather than a round number.
const portTestRefValue = "3zK9pQ2vX7mB4nL8sR1tYw"

// portTestAmount builds an amount as a struct literal, the way an adapter
// decoding a payload's minor units and currency would, so validation is
// exercised on values the money constructors never blessed.
func portTestAmount(minor int64, currency money.Currency) money.Amount {
	return money.Amount{Minor: minor, Currency: currency}
}

// portTestReport is a well-formed report the failure cases below break one
// rule at a time: an attributed, pending transaction with both amounts in
// one currency and the fragment it was normalised from.
func portTestReport() networks.Reported {
	return networks.Reported{
		ExternalID:   "awin-tx-90210",
		ClickRef:     networks.NewClickRef(portTestRefValue),
		StatusRaw:    "pending",
		Status:       networks.StatusPending,
		SaleAmount:   portTestAmount(12_500, portTestEUR),
		Commission:   portTestAmount(625, portTestEUR),
		TransactedAt: portTestAnchor,
		RawPayload:   json.RawMessage(`{"id":"90210","commissionStatus":"pending"}`),
	}
}

// portTestMerchant is a well-formed catalogue entry, broken one rule at a
// time in the same way.
func portTestMerchant() networks.ReportedMerchant {
	return networks.ReportedMerchant{
		ExternalID: "awin-adv-4471",
		Name:       "Gartenhaus GmbH",
		Country:    "DE",
		StatusRaw:  "joined",
		Status:     networks.MerchantStatusActive,
		RawPayload: json.RawMessage(`{"id":4471,"name":"Gartenhaus GmbH"}`),
	}
}

func TestReportedValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// report is either built in place or derived from the well-formed
		// base with exactly one rule broken.
		report networks.Reported
		// wantErr is the sentinel the refusal must wrap; nil means the
		// report is one a network could have produced.
		wantErr error
		// wantIn are fragments the message must carry.
		wantIn []string
	}{
		{
			name:   "an attributed pending transaction",
			report: portTestReport(),
		},
		{
			name: "an unattributed transaction is evidence too (FR-034)",
			report: func() networks.Reported {
				r := portTestReport()
				r.ClickRef = networks.ClickRef{}
				return r
			}(),
		},
		{
			name: "a reversal carrying a negative commission",
			report: func() networks.Reported {
				r := portTestReport()
				r.StatusRaw = "deleted"
				r.Status = networks.StatusReversed
				r.Commission = portTestAmount(-625, portTestEUR)
				return r
			}(),
		},
		{
			name: "a confirmed transaction paying nothing",
			report: func() networks.Reported {
				r := portTestReport()
				r.StatusRaw = "approved"
				r.Status = networks.StatusConfirmed
				r.Commission = portTestAmount(0, portTestEUR)
				return r
			}(),
		},
		{
			name: "a payload that is a JSON array fragment",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage(`[{"id":"90210"}]`)
				return r
			}(),
		},
		{
			name: "a payload carrying an escaped backslash, which the column stores as text",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage(`{"name":"Gartenhaus \\u0000 GmbH"}`)
				return r
			}(),
		},
		{
			name: "a payload carrying a well-formed surrogate pair",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage(`{"name":"\ud83d\ude00"}`)
				return r
			}(),
		},
		{
			name: "a declined transaction",
			report: func() networks.Reported {
				r := portTestReport()
				r.StatusRaw = "declined"
				r.Status = networks.StatusDeclined
				return r
			}(),
		},

		{
			name: "no external id",
			report: func() networks.Reported {
				r := portTestReport()
				r.ExternalID = ""
				return r
			}(),
			wantErr: networks.ErrMissingExternalID,
		},
		{
			name: "a whitespace external id",
			report: func() networks.Reported {
				r := portTestReport()
				r.ExternalID = " \t "
				return r
			}(),
			wantErr: networks.ErrMissingExternalID,
		},
		{
			name: "a click reference present and blank",
			report: func() networks.Reported {
				r := portTestReport()
				r.ClickRef = networks.NewClickRef("")
				return r
			}(),
			wantErr: networks.ErrBlankClickRef,
			wantIn:  []string{`"awin-tx-90210"`},
		},
		{
			name: "a click reference present and all space",
			report: func() networks.Reported {
				r := portTestReport()
				r.ClickRef = networks.NewClickRef("   ")
				return r
			}(),
			wantErr: networks.ErrBlankClickRef,
		},
		{
			name: "no verbatim status",
			report: func() networks.Reported {
				r := portTestReport()
				r.StatusRaw = ""
				return r
			}(),
			wantErr: networks.ErrMissingStatusRaw,
			wantIn:  []string{`"awin-tx-90210"`},
		},
		{
			name: "an all-space verbatim status, which the column refuses too",
			report: func() networks.Reported {
				r := portTestReport()
				r.StatusRaw = "   "
				return r
			}(),
			wantErr: networks.ErrMissingStatusRaw,
		},
		{
			name: "a status outside the closed set",
			report: func() networks.Reported {
				r := portTestReport()
				r.StatusRaw = "held_for_review"
				r.Status = networks.Status("held")
				return r
			}(),
			wantErr: networks.ErrUnmappableStatus,
			wantIn:  []string{`"held"`, `"held_for_review"`},
		},
		{
			name: "the zero status, which is what a forgotten mapping leaves",
			report: func() networks.Reported {
				r := portTestReport()
				r.StatusRaw = "approved"
				r.Status = ""
				return r
			}(),
			wantErr: networks.ErrUnmappableStatus,
		},
		{
			name: "a status that only differs in case",
			report: func() networks.Reported {
				r := portTestReport()
				r.Status = networks.Status("Pending")
				return r
			}(),
			wantErr: networks.ErrUnmappableStatus,
		},
		{
			name: "a sale amount with no currency",
			report: func() networks.Reported {
				r := portTestReport()
				r.SaleAmount = portTestAmount(12_500, "")
				return r
			}(),
			wantErr: money.ErrInvalidCurrency,
			wantIn:  []string{"sale amount"},
		},
		{
			name: "a commission with a malformed currency",
			report: func() networks.Reported {
				r := portTestReport()
				r.Commission = portTestAmount(625, "eur")
				return r
			}(),
			wantErr: money.ErrInvalidCurrency,
			wantIn:  []string{"commission"},
		},
		{
			name: "a sale and a commission in different currencies",
			report: func() networks.Reported {
				r := portTestReport()
				r.Commission = portTestAmount(625, portTestGBP)
				return r
			}(),
			wantErr: money.ErrCurrencyMismatch,
			wantIn:  []string{"one currency for both"},
		},
		{
			name: "no transaction time",
			report: func() networks.Reported {
				r := portTestReport()
				r.TransactedAt = time.Time{}
				return r
			}(),
			wantErr: networks.ErrMissingTransactedAt,
		},
		{
			name: "no raw payload (contract rule 1)",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = nil
				return r
			}(),
			wantErr: networks.ErrMissingRawPayload,
			wantIn:  []string{`"awin-tx-90210"`},
		},
		{
			name: "an empty raw payload",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage("")
				return r
			}(),
			wantErr: networks.ErrMissingRawPayload,
		},
		{
			name: "a whitespace raw payload",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage("  \n\t ")
				return r
			}(),
			wantErr: networks.ErrMissingRawPayload,
		},
		{
			name: "a raw payload of JSON null, which is absence in costume",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage("null")
				return r
			}(),
			wantErr: networks.ErrMissingRawPayload,
		},
		{
			name: "an empty object, which carries no more than the null does",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage("{}")
				return r
			}(),
			wantErr: networks.ErrMissingRawPayload,
		},
		{
			name: "an empty array",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage(" [ ] ")
				return r
			}(),
			wantErr: networks.ErrMissingRawPayload,
		},
		{
			name: "a bare scalar, which is nothing a fix could be re-derived from",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage(`""`)
				return r
			}(),
			wantErr: networks.ErrMissingRawPayload,
		},
		{
			name: "a bare number",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage(`0`)
				return r
			}(),
			wantErr: networks.ErrMissingRawPayload,
		},
		{
			name: "a raw payload that is not JSON",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage(`{"id":"90210"`)
				return r
			}(),
			wantErr: networks.ErrMalformedRawPayload,
		},
		{
			name: "a raw payload carrying a mis-encoded merchant name, which the column refuses",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage("{\"name\":\"Gartenh\xe4us\"}")
				return r
			}(),
			wantErr: networks.ErrMalformedRawPayload,
			wantIn:  []string{"UTF-8"},
		},
		{
			name: "a raw payload carrying a NUL escape, which jsonb cannot convert to text",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage(`{"name":"Gartenhaus\u0000"}`)
				return r
			}(),
			wantErr: networks.ErrMalformedRawPayload,
		},
		{
			name: "a raw payload carrying a lone high surrogate",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage(`{"name":"\ud83dx"}`)
				return r
			}(),
			wantErr: networks.ErrMalformedRawPayload,
		},
		{
			name: "a raw payload carrying a lone low surrogate",
			report: func() networks.Reported {
				r := portTestReport()
				r.RawPayload = json.RawMessage(`{"name":"\ude00"}`)
				return r
			}(),
			wantErr: networks.ErrMalformedRawPayload,
		},
		{
			name: "the external id is checked before everything else",
			report: networks.Reported{
				ExternalID: "",
				ClickRef:   networks.NewClickRef(" "),
				Status:     networks.Status("nonsense"),
			},
			wantErr: networks.ErrMissingExternalID,
		},
		{
			name: "the status is checked before the amounts",
			report: func() networks.Reported {
				r := portTestReport()
				r.Status = networks.Status("nonsense")
				r.SaleAmount = portTestAmount(1, "")
				return r
			}(),
			wantErr: networks.ErrUnmappableStatus,
		},
		{
			name:    "the zero report",
			report:  networks.Reported{},
			wantErr: networks.ErrMissingExternalID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			portTestAssert(t, "Reported.Validate()", tc.report.Validate(), tc.wantErr, tc.wantIn)
		})
	}
}

func TestReportedMerchantValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		merchant networks.ReportedMerchant
		wantErr  error
		wantIn   []string
	}{
		{
			name:     "an active retailer",
			merchant: portTestMerchant(),
		},
		{
			name: "a retailer bound to no single country",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.Country = ""
				return m
			}(),
		},
		{
			name: "a retailer who has left the network",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.StatusRaw = "notjoined"
				m.Status = networks.MerchantStatusLeftNetwork
				return m
			}(),
		},
		{
			name: "a paused route",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.StatusRaw = "suspended"
				m.Status = networks.MerchantStatusPaused
				return m
			}(),
		},

		{
			name: "no external merchant id",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.ExternalID = ""
				return m
			}(),
			wantErr: networks.ErrMissingExternalID,
		},
		{
			name: "an all-space external merchant id",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.ExternalID = " \t "
				return m
			}(),
			wantErr: networks.ErrMissingExternalID,
		},
		{
			name: "no name",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.Name = " "
				return m
			}(),
			wantErr: networks.ErrMissingMerchantName,
			wantIn:  []string{`"awin-adv-4471"`},
		},
		{
			name: "a lowercase country",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.Country = "de"
				return m
			}(),
			wantErr: networks.ErrInvalidMerchantCountry,
			wantIn:  []string{`"de"`},
		},
		{
			name: "a country lowercase only in its second letter",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.Country = "De"
				return m
			}(),
			wantErr: networks.ErrInvalidMerchantCountry,
			wantIn:  []string{`"De"`},
		},
		{
			name: "a three-letter country",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.Country = "DEU"
				return m
			}(),
			wantErr: networks.ErrInvalidMerchantCountry,
		},
		{
			name: "no verbatim status",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.StatusRaw = ""
				return m
			}(),
			wantErr: networks.ErrMissingStatusRaw,
		},
		{
			name: "an all-space verbatim status",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.StatusRaw = "  "
				return m
			}(),
			wantErr: networks.ErrMissingStatusRaw,
		},
		{
			name: "a route status outside the closed set",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.Status = networks.MerchantStatus("pending")
				return m
			}(),
			wantErr: networks.ErrUnmappableStatus,
			wantIn:  []string{`"pending"`, `"joined"`},
		},
		{
			name: "the zero route status, which is what a forgotten mapping leaves",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.Status = ""
				return m
			}(),
			wantErr: networks.ErrUnmappableStatus,
		},
		{
			name: "no raw payload",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.RawPayload = nil
				return m
			}(),
			wantErr: networks.ErrMissingRawPayload,
			wantIn:  []string{`"awin-adv-4471"`},
		},
		{
			name: "a raw payload of JSON null",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.RawPayload = json.RawMessage("null")
				return m
			}(),
			wantErr: networks.ErrMissingRawPayload,
		},
		{
			name: "a raw payload that is not JSON",
			merchant: func() networks.ReportedMerchant {
				m := portTestMerchant()
				m.RawPayload = json.RawMessage(`{"id":4471`)
				return m
			}(),
			wantErr: networks.ErrMalformedRawPayload,
		},
		{
			name:     "the zero catalogue entry",
			merchant: networks.ReportedMerchant{},
			wantErr:  networks.ErrMissingExternalID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			portTestAssert(t, "ReportedMerchant.Validate()", tc.merchant.Validate(), tc.wantErr, tc.wantIn)
		})
	}
}
