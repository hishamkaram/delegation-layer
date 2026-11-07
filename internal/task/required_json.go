package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Protocol fields without an omission option are mandatory, including zero-valued
// observations. Check the wire shape before encoding/json can turn absence or
// null into a zero value. Canonical names also eliminate struct-field aliases.
func validateRequiredFields(data []byte, target any) error {
	typ := reflect.TypeOf(target)
	if typ == nil || typ.Kind() != reflect.Pointer || reflect.ValueOf(target).IsNil() {
		return errors.New("strict JSON target must be a nonnil pointer")
	}
	return validateJSONType(data, typ.Elem())
}

func validateJSONType(data []byte, typ reflect.Type) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		return errors.New("null protocol field rejected")
	}
	if typ.Kind() == reflect.Pointer {
		return validateJSONType(data, typ.Elem())
	}
	// RawMessage is an explicitly opaque JSON value. The outer structure check
	// already validates its syntax and duplicate keys; its consumer validates shape.
	if typ == reflect.TypeFor[json.RawMessage]() {
		if !json.Valid(trimmed) {
			return errors.New("invalid raw JSON value")
		}
		return nil
	}
	if typ.Kind() == reflect.Struct {
		return validateJSONObject(data, typ)
	}
	// encoding/json represents []byte as a base64 string. Validate that
	// representation through its typed decoder; ordinary slice/array shape and
	// the historical numeric byte-array alternative retain their checks below.
	if typ == reflect.TypeFor[[]byte]() && len(trimmed) > 0 && trimmed[0] == '"' {
		var decoded []byte
		return json.Unmarshal(trimmed, &decoded)
	}
	if typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
		var entries []json.RawMessage
		if err := json.Unmarshal(data, &entries); err != nil {
			return err
		}
		for _, entry := range entries {
			if err := validateJSONType(entry, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateJSONObject(data []byte, typ reflect.Type) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for i := range typ.NumField() {
		field := typ.Field(i)
		if err := validateJSONField(fields, field); err != nil {
			return err
		}
	}
	for name := range fields {
		return fmt.Errorf("unknown or noncanonical JSON field %q", name)
	}
	return nil
}

func validateJSONField(fields map[string]json.RawMessage, field reflect.StructField) error {
	if !field.IsExported() {
		return nil
	}
	tag := strings.Split(field.Tag.Get("json"), ",")
	name := tag[0]
	if name == "-" {
		return nil
	}
	if name == "" {
		name = field.Name
	}
	value, present := fields[name]
	if !present {
		for _, option := range tag[1:] {
			if option == "omitempty" || option == "omitzero" {
				return nil
			}
		}
		return fmt.Errorf("missing required JSON field %q", name)
	}
	delete(fields, name)
	if err := validateJSONType(value, field.Type); err != nil {
		return fmt.Errorf("field %s: %w", name, err)
	}
	return nil
}
