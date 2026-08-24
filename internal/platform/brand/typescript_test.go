package brand

// The generator is tested from inside the package: the cases that matter
// most are the ones it must REFUSE, and refusing needs types the brand
// schema deliberately does not contain.

import (
	"reflect"
	"strings"
	"testing"
)

func TestTypeScriptTypesDeclaresTheWholeSchema(t *testing.T) {
	t.Parallel()

	got, err := TypeScriptTypes()
	if err != nil {
		t.Fatalf("TypeScriptTypes: %v", err)
	}

	if !strings.HasPrefix(got, TypeScriptHeader) {
		t.Error("the declaration does not open with the generated-file header")
	}

	// Every named type the schema reaches must be declared, or the
	// TypeScript side cannot describe a brand it can already parse.
	for _, name := range []string{
		"Brand", "Legal", "Document", "Domains", "Support",
		"Assets", "Theme", "Typography", "Defaults", "Payout",
	} {
		if !strings.Contains(got, "export interface "+name+" {") {
			t.Errorf("no declaration for %s", name)
		}
	}

	// A parent is declared before the types it refers to, so the file
	// reads in the order the schema nests.
	if strings.Index(got, "export interface Brand") > strings.Index(got, "export interface Legal") {
		t.Error("Brand is declared after a type it contains")
	}
	if strings.Index(got, "export interface Legal") > strings.Index(got, "export interface Document") {
		t.Error("Legal is declared after Document, which only it refers to")
	}

	// The field renderings that carry the mapping decisions: JSON names
	// rather than Go ones, readonly everywhere, and the two container
	// shapes the schema uses.
	for _, line := range []string{
		"  readonly id: string;",
		"  readonly legal: Legal;",
		"  readonly features: Readonly<Record<string, Readonly<Record<string, boolean>>>>;",
		"  readonly aliases: readonly string[];",
		"  readonly documents: Readonly<Record<string, Document>>;",
		"  readonly logoDark: string;",
		"  readonly headingWeight: number;",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("missing declaration line %q", line)
		}
	}

	// Go field names must not survive into the TypeScript: they are the
	// one place the two readers could disagree without noticing.
	if strings.Contains(got, "LogoDark") {
		t.Error("a Go field name leaked into the declaration")
	}
}

func TestTypeScriptTypesCarriesTheSchemaAsDataToo(t *testing.T) {
	t.Parallel()

	got, err := TypeScriptTypes()
	if err != nil {
		t.Fatalf("TypeScriptTypes: %v", err)
	}

	// Interfaces vanish at compile time and a brand file read from disk
	// is an `unknown`, so the same walk emits the schema again as data
	// for the loader to check a brand against.
	for _, want := range []string{
		"export const brandRoot = 'Brand';",
		"export const brandSchema: Readonly<Record<string, BrandInterfaceSchema>> = {",
		"  Brand: {",
		"    legal: { struct: 'Legal' },",
		"    features: { map: { map: 'boolean' } },",
		"  Domains: {",
		"    aliases: { list: 'string' },",
		"  Legal: {",
		"    documents: { map: { struct: 'Document' } },",
		"  Typography: {",
		"    headingWeight: 'number',",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the runtime schema is missing %q", want)
		}
	}

	// Every interface declared above must also appear as data: a field
	// the checker cannot see is a field nothing checks.
	for _, name := range []string{
		"Brand", "Legal", "Document", "Domains", "Support",
		"Assets", "Theme", "Typography", "Defaults", "Payout",
	} {
		if !strings.Contains(got, "\n  "+name+": {\n") {
			t.Errorf("%s is declared as a type but not as schema data", name)
		}
	}
}

func TestTypeScriptSchemaRendersTheKindsTheSchemaUses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  reflect.Type
		want string
	}{
		{name: "string", typ: reflect.TypeOf(""), want: "'string'"},
		{name: "bool", typ: reflect.TypeOf(true), want: "'boolean'"},
		{name: "int", typ: reflect.TypeOf(0), want: "'number'"},
		{name: "float64", typ: reflect.TypeOf(0.0), want: "'number'"},
		{name: "slice", typ: reflect.TypeOf([]string{}), want: "{ list: 'string' }"},
		{name: "map", typ: reflect.TypeOf(map[string]bool{}), want: "{ map: 'boolean' }"},
		{
			name: "map of maps",
			typ:  reflect.TypeOf(map[string]map[string]bool{}),
			want: "{ map: { map: 'boolean' } }",
		},
		{name: "named struct", typ: reflect.TypeOf(Document{}), want: "{ struct: 'Document' }"},
		{
			name: "map of named structs",
			typ:  reflect.TypeOf(map[string]Document{}),
			want: "{ map: { struct: 'Document' } }",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := typeScriptSchema(testCase.typ); got != testCase.want {
				t.Errorf("typeScriptSchema(%s) = %q, want %q", testCase.typ, got, testCase.want)
			}
		})
	}
}

func TestTypeScriptTypesIsDeterministic(t *testing.T) {
	t.Parallel()

	// Map iteration order is random in Go; a generator that inherited it
	// would fail its own drift check at random.
	first, err := TypeScriptTypes()
	if err != nil {
		t.Fatalf("TypeScriptTypes: %v", err)
	}
	for range 8 {
		again, err := TypeScriptTypes()
		if err != nil {
			t.Fatalf("TypeScriptTypes: %v", err)
		}
		if again != first {
			t.Fatal("two runs of the generator disagreed")
		}
	}
}

func TestTypeScriptTypeRendersTheKindsTheSchemaUses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  reflect.Type
		want string
	}{
		{name: "string", typ: reflect.TypeOf(""), want: "string"},
		{name: "bool", typ: reflect.TypeOf(true), want: "boolean"},
		{name: "int", typ: reflect.TypeOf(0), want: "number"},
		{name: "int64", typ: reflect.TypeOf(int64(0)), want: "number"},
		{name: "uint8", typ: reflect.TypeOf(uint8(0)), want: "number"},
		{name: "float64", typ: reflect.TypeOf(0.0), want: "number"},
		{name: "slice", typ: reflect.TypeOf([]string{}), want: "readonly string[]"},
		{name: "map", typ: reflect.TypeOf(map[string]bool{}), want: "Readonly<Record<string, boolean>>"},
		{
			name: "map of maps",
			typ:  reflect.TypeOf(map[string]map[string]bool{}),
			want: "Readonly<Record<string, Readonly<Record<string, boolean>>>>",
		},
		{name: "named struct", typ: reflect.TypeOf(Document{}), want: "Document"},
		{name: "slice of named structs", typ: reflect.TypeOf([]Document{}), want: "readonly Document[]"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, err := typeScriptType(testCase.typ)
			if err != nil {
				t.Fatalf("typeScriptType(%s): %v", testCase.typ, err)
			}
			if got != testCase.want {
				t.Errorf("typeScriptType(%s) = %q, want %q", testCase.typ, got, testCase.want)
			}
		})
	}
}

func TestTypeScriptTypeRefusesWhatItCannotPromise(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  reflect.Type
		want string
	}{
		{name: "channel", typ: reflect.TypeOf(make(chan int)), want: "unsupported kind chan"},
		{name: "pointer", typ: reflect.TypeOf(&Document{}), want: "unsupported kind ptr"},
		{name: "non-string map key", typ: reflect.TypeOf(map[int]string{}), want: "map key int is not a string"},
		{name: "slice of the unsupported", typ: reflect.TypeOf([]chan int{}), want: "unsupported kind chan"},
		{name: "map of the unsupported", typ: reflect.TypeOf(map[string]chan int{}), want: "unsupported kind chan"},
		{
			name: "anonymous struct",
			typ:  reflect.TypeOf(struct{ Field string }{}),
			want: "has no name to declare",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := typeScriptType(testCase.typ)
			if err == nil {
				t.Fatalf("typeScriptType(%s) invented a TypeScript type", testCase.typ)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("error %q does not mention %q", err, testCase.want)
			}
		})
	}
}

// unrenderable is a struct the generator must refuse rather than
// half-declare.
type unrenderable struct {
	Fine    string   `json:"fine"`
	Channel chan int `json:"channel"`
}

// tagged exercises every json tag shape the encoder understands, so the
// declaration names fields exactly as the brand file does.
type tagged struct {
	Tagged    string `json:"tagged"`
	WithOpts  string `json:"withOpts,omitempty"`
	Untagged  string
	OptsOnly  string `json:",omitempty"`
	Skipped   string `json:"-"`
	unxported string //nolint:unused // present so the generator can skip it
}

func TestDeclarationsNameFieldsTheWayTheEncoderDoes(t *testing.T) {
	t.Parallel()

	got, err := typeScriptDeclarations(reflect.TypeOf(tagged{}))
	if err != nil {
		t.Fatalf("typeScriptDeclarations: %v", err)
	}

	for _, want := range []string{
		"  readonly tagged: string;",
		"  readonly withOpts: string;",
		"  readonly Untagged: string;",
		"  readonly OptsOnly: string;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, unwanted := range []string{"Skipped", "unxported"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("declared %q, which the JSON encoder never writes", unwanted)
		}
	}
}

func TestDeclarationsNameTheOffendingField(t *testing.T) {
	t.Parallel()

	_, err := typeScriptDeclarations(reflect.TypeOf(unrenderable{}))
	if err == nil {
		t.Fatal("typeScriptDeclarations accepted a type it cannot render")
	}
	if want := "unrenderable.Channel"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the field as %q", err, want)
	}
}

// nested proves a type reached twice is declared once, and that the
// failure of a nested type is reported rather than swallowed.
type nested struct {
	First  Document     `json:"first"`
	Second Document     `json:"second"`
	Broken unrenderable `json:"broken"`
}

func TestDeclarationsEmitEachTypeOnceAndPropagateNestedFailures(t *testing.T) {
	t.Parallel()

	_, err := typeScriptDeclarations(reflect.TypeOf(nested{}))
	if err == nil {
		t.Fatal("a broken nested type was accepted")
	}
	if want := "unrenderable.Channel"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the nested field as %q", err, want)
	}

	type twice struct {
		First  Document `json:"first"`
		Second Document `json:"second"`
	}
	declared, err := typeScriptDeclarations(reflect.TypeOf(twice{}))
	if err != nil {
		t.Fatalf("typeScriptDeclarations: %v", err)
	}
	if count := strings.Count(declared, "export interface Document {"); count != 1 {
		t.Errorf("Document declared %d times, want once", count)
	}
}
