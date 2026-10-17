package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

const (
	orderedJSONScalar byte = iota
	orderedJSONObject
	orderedJSONArray
)

type orderedJSON struct {
	kind   byte
	scalar json.RawMessage
	fields []orderedJSONField
	items  []orderedJSON
}

type orderedJSONField struct {
	key   string
	value orderedJSON
}

func parseOrderedJSON(raw []byte) (orderedJSON, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeOrderedJSON(decoder)
	if err != nil {
		return orderedJSON{}, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return orderedJSON{}, fmt.Errorf("trailing JSON value")
		}
		return orderedJSON{}, err
	}
	return value, nil
}

func decodeOrderedJSON(decoder *json.Decoder) (orderedJSON, error) {
	token, err := decoder.Token()
	if err != nil {
		return orderedJSON{}, err
	}
	switch typed := token.(type) {
	case json.Delim:
		switch typed {
		case '{':
			return decodeOrderedJSONObject(decoder)
		case '[':
			return decodeOrderedJSONArray(decoder)
		default:
			return orderedJSON{}, fmt.Errorf("unexpected JSON delimiter %q", typed)
		}
	case string, bool, json.Number:
		encoded, marshalErr := json.Marshal(typed)
		if marshalErr != nil {
			return orderedJSON{}, marshalErr
		}
		return orderedJSON{kind: orderedJSONScalar, scalar: encoded}, nil
	case nil:
		return orderedJSON{kind: orderedJSONScalar, scalar: []byte("null")}, nil
	default:
		return orderedJSON{}, fmt.Errorf("unsupported JSON token %T", token)
	}
}

func decodeOrderedJSONObject(decoder *json.Decoder) (orderedJSON, error) {
	fields := make([]orderedJSONField, 0)
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return orderedJSON{}, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return orderedJSON{}, fmt.Errorf("JSON object key is not a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return orderedJSON{}, fmt.Errorf("duplicate JSON object key %q", key)
		}
		seen[key] = struct{}{}
		value, err := decodeOrderedJSON(decoder)
		if err != nil {
			return orderedJSON{}, err
		}
		fields = append(fields, orderedJSONField{key: key, value: value})
	}
	close, err := decoder.Token()
	if err != nil {
		return orderedJSON{}, err
	}
	if close != json.Delim('}') {
		return orderedJSON{}, fmt.Errorf("JSON object was not closed")
	}
	return orderedJSON{kind: orderedJSONObject, fields: fields}, nil
}

func decodeOrderedJSONArray(decoder *json.Decoder) (orderedJSON, error) {
	items := make([]orderedJSON, 0)
	for decoder.More() {
		value, err := decodeOrderedJSON(decoder)
		if err != nil {
			return orderedJSON{}, err
		}
		items = append(items, value)
	}
	close, err := decoder.Token()
	if err != nil {
		return orderedJSON{}, err
	}
	if close != json.Delim(']') {
		return orderedJSON{}, fmt.Errorf("JSON array was not closed")
	}
	return orderedJSON{kind: orderedJSONArray, items: items}, nil
}

func (value orderedJSON) encode() ([]byte, error) {
	switch value.kind {
	case orderedJSONScalar:
		return slices.Clone(value.scalar), nil
	case orderedJSONArray:
		encoded := []byte{'['}
		for index, item := range value.items {
			if index > 0 {
				encoded = append(encoded, ',')
			}
			itemEncoded, err := item.encode()
			if err != nil {
				return nil, err
			}
			encoded = append(encoded, itemEncoded...)
		}
		return append(encoded, ']'), nil
	case orderedJSONObject:
		encoded := []byte{'{'}
		for index, field := range value.fields {
			if index > 0 {
				encoded = append(encoded, ',')
			}
			key, err := json.Marshal(field.key)
			if err != nil {
				return nil, err
			}
			encoded = append(encoded, key...)
			encoded = append(encoded, ':')
			valueEncoded, err := field.value.encode()
			if err != nil {
				return nil, err
			}
			encoded = append(encoded, valueEncoded...)
		}
		return append(encoded, '}'), nil
	default:
		return nil, fmt.Errorf("unknown ordered JSON value kind %d", value.kind)
	}
}

func jsonFalse(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("false"))
}

func jsonObjectBoolFalse(raw json.RawMessage, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return false
	}
	return jsonFalse(object[key])
}
