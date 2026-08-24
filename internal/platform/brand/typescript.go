package brand

import (
	"fmt"
	"reflect"
	"strings"
)

// TypeScriptHeader opens the generated declaration. It names where the
// schema actually lives so that the first instinct on reading the
// generated file is to go and change the Go types instead of it.
const TypeScriptHeader = `// Code generated from internal/platform/brand/brand.go. DO NOT EDIT.
//
// The brand schema is defined exactly once, as the Go types in
// internal/platform/brand/brand.go, and this declaration is derived from
// them. Two readers of one brand file cannot disagree about its shape if
// only one of them is allowed to describe it.
//
// The prose explaining what each field means, and why it is required,
// lives beside those Go types. Regenerate with:
//
//   go test ./internal/platform/brand/ -run TypeScriptDeclaration -update
`

// typeScriptSchemaPreamble introduces the runtime half of the generated
// file. The interfaces above it vanish at compile time, and a brand file
// read from disk is an `unknown` that no interface can vouch for — so the
// same walk emits the schema a second time as data, and web/src/lib/brand
// checks a loaded brand against it with one generic walker rather than a
// hand-written check per field.
const typeScriptSchemaPreamble = `
/**
 * How one field is shaped, as data rather than as a type: a primitive by
 * name, another interface, a list, or a string-keyed map.
 */
export type BrandFieldType =
  | 'string'
  | 'number'
  | 'boolean'
  | { readonly struct: string }
  | { readonly list: BrandFieldType }
  | { readonly map: BrandFieldType };

/** One interface's fields, by the name they carry in the brand file. */
export type BrandInterfaceSchema = Readonly<Record<string, BrandFieldType>>;

/** The interface a whole brand file is checked against. */
export const brandRoot = 'Brand';
`

// TypeScriptTypes returns the TypeScript declaration of the brand schema,
// derived by reflection from Brand and everything it reaches: an
// interface per named type, and the same schema again as data for
// checking a brand file at run time.
//
// It exists so that web/src/lib/brand does not restate the schema in a
// second language, where it would drift on the first field anyone adds to
// only one side. The committed declaration is checked against this output
// in CI, so drift is a failing test rather than a rendering surprise.
func TypeScriptTypes() (string, error) {
	return typeScriptDeclarations(reflect.TypeOf(Brand{}))
}

// tsField is one field of one interface, rendered both ways: as the
// TypeScript type it is declared with, and as the schema literal that
// describes it at run time.
type tsField struct {
	name   string
	typ    string
	schema string
}

// tsInterface is one named Go struct, ready to render.
type tsInterface struct {
	name   string
	fields []tsField
}

// typeScriptDeclarations renders every interface reachable from root,
// followed by the runtime schema.
func typeScriptDeclarations(root reflect.Type) (string, error) {
	interfaces, err := collectInterfaces(root)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	out.WriteString(TypeScriptHeader)
	for _, iface := range interfaces {
		fmt.Fprintf(&out, "\nexport interface %s {\n", iface.name)
		for _, field := range iface.fields {
			fmt.Fprintf(&out, "  readonly %s: %s;\n", field.name, field.typ)
		}
		out.WriteString("}\n")
	}

	out.WriteString(typeScriptSchemaPreamble)
	out.WriteString("\n/** Every interface in the schema, by name. */\nexport const brandSchema: Readonly<Record<string, BrandInterfaceSchema>> = {\n")
	for _, iface := range interfaces {
		fmt.Fprintf(&out, "  %s: {\n", iface.name)
		for _, field := range iface.fields {
			fmt.Fprintf(&out, "    %s: %s,\n", field.name, field.schema)
		}
		out.WriteString("  },\n")
	}
	out.WriteString("};\n")

	return out.String(), nil
}

// collectInterfaces walks root depth-first and returns one entry per
// named struct it reaches, parents before the types they refer to, so the
// generated file reads in the order the schema is nested. A type reached
// twice is collected once.
func collectInterfaces(root reflect.Type) ([]tsInterface, error) {
	var collected []tsInterface
	seen := map[string]bool{}

	var visit func(t reflect.Type) error
	visit = func(t reflect.Type) error {
		if seen[t.Name()] {
			return nil
		}
		seen[t.Name()] = true

		iface := tsInterface{name: t.Name()}
		var children []reflect.Type
		for i := range t.NumField() {
			field := t.Field(i)
			name, ok := jsonName(field)
			if !ok {
				continue
			}
			typ, err := typeScriptType(field.Type)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", t.Name(), field.Name, err)
			}
			iface.fields = append(iface.fields, tsField{
				name:   name,
				typ:    typ,
				schema: typeScriptSchema(field.Type),
			})
			children = append(children, namedStructs(field.Type)...)
		}
		collected = append(collected, iface)

		for _, child := range children {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}

	if err := visit(root); err != nil {
		return nil, err
	}
	return collected, nil
}

// jsonName is the field's name in the brand file, and whether it appears
// there at all. A field the JSON encoder skips must not appear in the
// TypeScript either, or the two readers stop agreeing.
func jsonName(field reflect.StructField) (string, bool) {
	if !field.IsExported() {
		return "", false
	}
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return field.Name, true
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return "", false
	}
	if name == "" {
		return field.Name, true
	}
	return name, true
}

// typeScriptType renders one Go type as TypeScript. Only the constructs
// the brand schema is built from are handled; anything else is an error
// rather than a guess, because a guessed type is a compile-time promise
// nobody checked.
func typeScriptType(t reflect.Type) (string, error) {
	switch t.Kind() {
	case reflect.String:
		return "string", nil
	case reflect.Bool:
		return "boolean", nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number", nil
	case reflect.Slice:
		elem, err := typeScriptType(t.Elem())
		if err != nil {
			return "", err
		}
		return "readonly " + elem + "[]", nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return "", fmt.Errorf("map key %s is not a string", t.Key())
		}
		elem, err := typeScriptType(t.Elem())
		if err != nil {
			return "", err
		}
		return "Readonly<Record<string, " + elem + ">>", nil
	case reflect.Struct:
		if t.Name() == "" {
			return "", fmt.Errorf("anonymous struct %s has no name to declare", t)
		}
		return t.Name(), nil
	default:
		return "", fmt.Errorf("unsupported kind %s", t.Kind())
	}
}

// typeScriptSchema renders the same type as the runtime schema literal.
// It is only ever called for types typeScriptType has already accepted,
// which is why the default arm can be the numeric kinds: everything else
// failed one function earlier.
func typeScriptSchema(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "'string'"
	case reflect.Bool:
		return "'boolean'"
	case reflect.Slice:
		return "{ list: " + typeScriptSchema(t.Elem()) + " }"
	case reflect.Map:
		return "{ map: " + typeScriptSchema(t.Elem()) + " }"
	case reflect.Struct:
		return "{ struct: '" + t.Name() + "' }"
	default:
		return "'number'"
	}
}

// namedStructs returns the named struct types reachable from t without
// passing through another named struct — the children whose declarations
// must follow t's own. Every struct it sees has a name, because
// typeScriptType is asked about the same field first and refuses the
// anonymous ones.
func namedStructs(t reflect.Type) []reflect.Type {
	switch t.Kind() {
	case reflect.Struct:
		return []reflect.Type{t}
	case reflect.Slice, reflect.Map:
		return namedStructs(t.Elem())
	default:
		return nil
	}
}
