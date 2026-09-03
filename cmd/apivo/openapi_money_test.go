package main

// C-6 on the wire: every amount the API describes is {minor, currency}, and
// no decimal ever crosses the boundary (T125).
//
// The invariant is enforced in the schema and in Go by money.Amount, and the
// handlers marshal that type - so today the document is right by
// construction. What this file guards is the next hand-written field: an
// OpenAPI document is edited by people, and "balance": {"type": "number"} is
// one plausible keystroke away from describing a float the frontend would
// then parse as one. The three failure modes are distinct and each has its
// own case below: a decimal type, a bare integer of minor units with no
// currency beside it, and money as a string.
//
// It reads the document the server SERVES rather than the file on disk, for
// the reason openapi_routes_test.go does: what a client receives is the
// artefact under test.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// moneyRef is what a money-bearing field must resolve to. One shared schema,
// referenced - never re-declared inline, because a second declaration is a
// second rule, and the two would drift.
const moneyRef = "#/components/schemas/Money"

// moneyNames are the property names that carry money in this API, exactly.
// Matched exactly rather than by substring: "expected" is a Difference's
// expected commission and must be Money, while "expected_confirmation_at" is
// a timestamp, and a substring match would demand the wrong thing of it.
var moneyNames = []string{
	"actual", "amount", "commission", "confirmed", "delta", "expected",
	"paid", "paid_out", "payout_threshold", "pending", "reserved",
	"rounding", "sale", "balance",
}

// moneySuffixes catch the compound names - sale_amount, cashback_amount,
// released_amount - without listing every one of them, and catch the two
// spellings a bare integer of minor units would arrive under.
var moneySuffixes = []string{"_amount", "_minor", "_cents"}

// carriesMoney reports whether a property name denotes an amount of money.
func carriesMoney(name string) bool {
	if slices.Contains(moneyNames, name) {
		return true
	}
	for _, suffix := range moneySuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// rawDocument is the served document as a tree, because this test walks
// everything rather than reading a known shape.
func rawDocument(t *testing.T) map[string]any {
	t.Helper()
	srv := platformhttp.New(discardLogger(), ":0", "", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/openapi.json = %d, want %d", rec.Code, http.StatusOK)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served document does not parse: %v", err)
	}
	return doc
}

// walk visits every object in the document, handing each one the JSON
// pointer that reaches it so a failure names the field rather than the fact
// that some field somewhere is wrong.
func walk(node any, path string, visit func(obj map[string]any, path string)) {
	switch n := node.(type) {
	case map[string]any:
		visit(n, path)
		for key, value := range n {
			walk(value, path+"/"+key, visit)
		}
	case []any:
		for i, value := range n {
			walk(value, path+"/"+itoa(i), visit)
		}
	}
}

// itoa keeps the pointer arithmetic out of walk.
func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}

// resolvesToMoney reports whether a property schema is the shared Money
// schema: referenced directly, wrapped in a single-element allOf so a
// description can be attached, or offered beside null where the field is
// optional. Those are the three shapes the document actually uses, and
// anything else is a fourth spelling of money that nobody reviewed.
func resolvesToMoney(schema map[string]any) bool {
	if ref, ok := schema["$ref"].(string); ok && ref == moneyRef {
		return true
	}
	for _, keyword := range []string{"allOf", "oneOf", "anyOf"} {
		branches, ok := schema[keyword].([]any)
		if !ok {
			continue
		}
		for _, branch := range branches {
			member, ok := branch.(map[string]any)
			if !ok {
				continue
			}
			if ref, ok := member["$ref"].(string); ok && ref == moneyRef {
				return true
			}
		}
	}
	return false
}

// TestTheMoneySchemaIsTheOneC6Describes. Everything else here defers to this
// schema, so if it is wrong every amount in the API is wrong with it.
func TestTheMoneySchemaIsTheOneC6Describes(t *testing.T) {
	t.Parallel()
	doc := rawDocument(t)

	schemas, ok := doc["components"].(map[string]any)["schemas"].(map[string]any)
	if !ok {
		t.Fatal("the document has no components.schemas")
	}
	money, ok := schemas["Money"].(map[string]any)
	if !ok {
		t.Fatal("the document declares no Money schema, so nothing can reference one")
	}

	if money["type"] != "object" {
		t.Errorf("Money is %v, want an object", money["type"])
	}
	// Both halves required. A minor unit without its currency is a number
	// whose meaning depends on a convention nobody wrote down.
	required, _ := money["required"].([]any)
	var names []string
	for _, r := range required {
		if s, ok := r.(string); ok {
			names = append(names, s)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"currency", "minor"}) {
		t.Errorf("Money requires %v, want both currency and minor", names)
	}
	// Closed, so a third field cannot appear beside them and be believed.
	if closed, ok := money["additionalProperties"].(bool); !ok || closed {
		t.Errorf("Money additionalProperties = %v, want false", money["additionalProperties"])
	}

	props, _ := money["properties"].(map[string]any)
	minor, _ := props["minor"].(map[string]any)
	if minor["type"] != "integer" {
		t.Errorf("Money.minor is %v, want an integer (C-6: minor units, never a decimal)", minor["type"])
	}
	if minor["format"] != "int64" {
		t.Errorf("Money.minor format = %v, want int64", minor["format"])
	}
	currency, _ := props["currency"].(map[string]any)
	if currency["type"] != "string" {
		t.Errorf("Money.currency is %v, want a string", currency["type"])
	}
	// An explicit ISO-4217 shape, so "EURO" or "eur" is refused by the
	// contract rather than by whatever reads it later.
	if currency["pattern"] != "^[A-Z]{3}$" {
		t.Errorf("Money.currency pattern = %v, want ^[A-Z]{3}$", currency["pattern"])
	}
}

// TestNoDecimalCrossesTheAPIBoundary is C-6's own sentence, checked over the
// whole document: no field is a float, in any schema, parameter or example.
func TestNoDecimalCrossesTheAPIBoundary(t *testing.T) {
	t.Parallel()

	var offenders []string
	walk(rawDocument(t), "", func(obj map[string]any, path string) {
		switch declared := obj["type"].(type) {
		case string:
			if declared == "number" {
				offenders = append(offenders, path+" is type number")
			}
		case []any:
			for _, one := range declared {
				if one == "number" {
					offenders = append(offenders, path+" has number among its types")
				}
			}
		}
		// A float format on an integer is the same mistake wearing the
		// other half of the disguise.
		if format, ok := obj["format"].(string); ok {
			if format == "float" || format == "double" || format == "decimal" {
				offenders = append(offenders, path+" has format "+format)
			}
		}
	})
	if len(offenders) != 0 {
		t.Errorf("the document describes %d floating-point field(s); C-6 says no decimal ever crosses an API boundary:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// TestOnlyTheMoneySchemaDeclaresMinorUnits. An inline {minor, currency} would
// be a second declaration of the one shape, free to drift from it - and an
// inline object with a minor and no currency would be the bare integer C-6
// exists to forbid.
func TestOnlyTheMoneySchemaDeclaresMinorUnits(t *testing.T) {
	t.Parallel()

	var declarers []string
	walk(rawDocument(t), "", func(obj map[string]any, path string) {
		props, ok := obj["properties"].(map[string]any)
		if !ok {
			return
		}
		if _, has := props["minor"]; has {
			declarers = append(declarers, path)
		}
	})
	if !slices.Equal(declarers, []string{"/components/schemas/Money"}) {
		t.Errorf("minor units are declared at %v, want only /components/schemas/Money; reference the shared schema instead of re-declaring it", declarers)
	}
}

// TestEveryAmountResolvesToTheMoneySchema is the case that catches the field
// added next: a name that carries money, typed as anything else.
func TestEveryAmountResolvesToTheMoneySchema(t *testing.T) {
	t.Parallel()

	type field struct{ path, shape string }
	var wrong []field
	seen := 0
	walk(rawDocument(t), "", func(obj map[string]any, path string) {
		props, ok := obj["properties"].(map[string]any)
		if !ok {
			return
		}
		for name, raw := range props {
			if !carriesMoney(name) {
				continue
			}
			schema, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			// Money's own minor and currency are the shape, not users of it.
			if path == "/components/schemas/Money" {
				continue
			}
			seen++
			if !resolvesToMoney(schema) {
				rendered, _ := json.Marshal(schema)
				wrong = append(wrong, field{path: path + "/properties/" + name, shape: string(rendered)})
			}
		}
	})

	for _, f := range wrong {
		t.Errorf("%s carries money but is %s; it must reference %s", f.path, f.shape, moneyRef)
	}
	// A guard on the guard: if the walk stopped finding money-bearing
	// fields, this test would pass by asserting nothing at all.
	if seen < 20 {
		t.Errorf("only %d money-bearing field(s) were examined; the document describes far more, so the walk is not reaching them", seen)
	}
}
