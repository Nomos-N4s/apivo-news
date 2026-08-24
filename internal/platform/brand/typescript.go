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
// lives beside those Go types.
`

// TypeScriptTypes returns the TypeScript declaration of the brand schema,
// derived by reflection from Brand and everything it reaches.
//
// It exists so that web/src/lib/brand does not restate the schema in a
// second language, where it would drift on the first field anyone adds to
// only one side. The committed declaration is checked against this output
// in CI, so drift is a failing test rather than a rendering surprise.
func TypeScriptTypes() (string, error) {
	return typeScriptDeclarations(reflect.TypeOf(Brand{}))
}

// typeScriptDeclarations walks root depth-first and emits one exported
// interface per named struct it reaches, parents before the types they
// refer to, so the file reads in the order the schema is nested.
func typeScriptDeclarations(root reflect.Type) (string, error) {
	var out strings.Builder
	out.WriteString(TypeScriptHeader)

	emitted := map[string]bool{}
	var emit func(t reflect.Type) error
	emit = func(t reflect.Type) error {
		if emitted[t.Name()] {
			return nil
		}
		emitted[t.Name()] = true

		var body strings.Builder
		var children []reflect.Type
		for i := range t.NumField() {
			field := t.Field(i)
			name, ok := jsonName(field)
			if !ok {
				continue
			}
			rendered, err := typeScriptType(field.Type)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", t.Name(), field.Name, err)
			}
			fmt.Fprintf(&body, "  readonly %s: %s;\n", name, rendered)
			children = append(children, namedStructs(field.Type)...)
		}

		fmt.Fprintf(&out, "\nexport interface %s {\n%s}\n", t.Name(), body.String())
		for _, child := range children {
			if err := emit(child); err != nil {
				return err
			}
		}
		return nil
	}

	if err := emit(root); err != nil {
		return "", err
	}
	return out.String(), nil
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
