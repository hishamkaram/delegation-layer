package task

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type zeroOptionalJSON struct {
	Count int `json:"count,omitzero"`
}

type combinedOptionalJSON struct {
	Count int `json:"count,omitempty,string"`
}

type combinedZeroJSON struct {
	Count int `json:"count,string,omitzero,omitempty"`
}

type defaultNameZeroJSON struct {
	Count int `json:",omitzero"`
}

func TestDecodeStrictOptionalJSONOptions(t *testing.T) {
	t.Run("zero omitted", assertOptionalJSONAbsent[zeroOptionalJSON])
	t.Run("combined empty", assertOptionalJSONAbsent[combinedOptionalJSON])
	t.Run("combined zero", assertOptionalJSONAbsent[combinedZeroJSON])
	t.Run("default field name", assertOptionalJSONAbsent[defaultNameZeroJSON])
}

func TestDecodeStrictZeroOptionalCanonicalBytes(t *testing.T) {
	data, err := MarshalCanonical(zeroOptionalJSON{})
	if err != nil || string(data) != "{}\n" {
		t.Fatalf("zero optional field changed canonical empty-object bytes: %q, %v", data, err)
	}
	var value zeroOptionalJSON
	if err = DecodeStrict(data, &value); err != nil {
		t.Fatalf("canonical encoder output cannot be strictly decoded: %v", err)
	}
}

func assertOptionalJSONAbsent[T comparable](t *testing.T) {
	t.Helper()
	var value T
	if err := DecodeStrict([]byte("{}"), &value); err != nil {
		t.Fatalf("encoder-omitted field was required: %v", err)
	}
}

type requiredJSONChild struct {
	Name string `json:"name"`
}

type optionalJSONChild struct {
	Child requiredJSONChild `json:"child,omitzero"`
}

func TestDecodeStrictOptionalJSONRetainsProvidedFieldValidation(t *testing.T) {
	for _, test := range []struct {
		name, data, detail string
	}{
		{"null", `{"child":null}`, "null protocol field"},
		{"nested missing", `{"child":{}}`, "missing required JSON field"},
		{"nested null", `{"child":{"name":null}}`, "null protocol field"},
		{"unknown", `{"child":{"name":"ok","other":1}}`, "unknown or noncanonical"},
		{"alias", `{"Child":{"name":"ok"}}`, "unknown or noncanonical"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var value optionalJSONChild
			err := DecodeStrict([]byte(test.data), &value)
			if err == nil || !strings.Contains(err.Error(), test.detail) {
				t.Fatalf("invalid optional field accepted or misclassified: %v", err)
			}
			if value != (optionalJSONChild{}) {
				t.Fatal("invalid wire shape partially decoded")
			}
		})
	}
	var value optionalJSONChild
	if err := DecodeStrict([]byte(`{"child":{"name":"ok"}}`), &value); err != nil || value.Child.Name != "ok" {
		t.Fatalf("valid provided optional object rejected: %v", err)
	}
}

type requiredStringJSON struct {
	Count int `json:"count,string"`
}

func TestDecodeStrictOtherJSONOptionsDoNotMakeFieldsOptional(t *testing.T) {
	assertRequiredJSONAbsent[requiredStringJSON](t)
	// Malformed tag text is metadata under test, not a maintained Go schema.
	field := reflect.TypeFor[requiredStringJSON]().Field(0)
	field.Tag = reflect.StructTag(`json:"count,omit_zero"`)
	if err := validateJSONField(map[string]json.RawMessage{}, field); err == nil || err.Error() != `missing required JSON field "count"` {
		t.Fatalf("misspelled omission option waived a required field: %v", err)
	}
}

func assertRequiredJSONAbsent[T comparable](t *testing.T) {
	t.Helper()
	var value T
	if err := DecodeStrict([]byte("{}"), &value); err == nil || !strings.Contains(err.Error(), "missing required JSON field") {
		t.Fatalf("required field lost its presence check: %v", err)
	}
}
