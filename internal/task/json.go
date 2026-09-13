package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode"
)

// ComputeSHA256 computes the lowercase hex-encoded SHA-256 digest of data.
func ComputeSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// MarshalCanonical serializes v into canonical JSON format followed by a single newline.
func MarshalCanonical(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonical json marshal: %w", err)
	}
	data = append(data, '\n')
	return data, nil
}

// ValidateJSONStructure verifies that data does not exceed 1 MiB, is valid JSON,
// has no duplicate keys (including case-folded aliases) at any nesting level,
// and contains no trailing bytes.
func ValidateJSONStructure(data []byte) error {
	if len(data) > MaxControlRecordSize {
		return ErrControlRecordTooBig
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return errors.New("empty JSON data")
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return errors.New("null JSON root rejected")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decoding json token: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return fmt.Errorf("expected top-level JSON object or array, got %v", tok)
	}
	if err := checkTokenValue(dec, tok); err != nil {
		return err
	}
	if dec.More() {
		return ErrTrailingJSON
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrTrailingJSON
	}
	return nil
}

func checkTokenValue(dec *json.Decoder, tok json.Token) error {
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		return checkObject(dec)
	case '[':
		return checkArray(dec)
	default:
		return fmt.Errorf("unexpected json delimiter: %v", delim)
	}
}

func checkObject(dec *json.Decoder) error {
	seen := make(map[string]struct{})
	seenLower := make(map[string]string)
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			return err
		}
		k, ok := ktok.(string)
		if !ok {
			return fmt.Errorf("expected string key, got %T", ktok)
		}
		if _, exists := seen[k]; exists {
			return fmt.Errorf("%w: %q", ErrDuplicateKey, k)
		}
		lower := foldJSONKey(k)
		if prev, exists := seenLower[lower]; exists {
			return fmt.Errorf("%w: case-folded alias %q / %q", ErrDuplicateKey, prev, k)
		}
		seen[k] = struct{}{}
		seenLower[lower] = k
		vtok, err := dec.Token()
		if err != nil {
			return err
		}
		if err := checkTokenValue(dec, vtok); err != nil {
			return err
		}
	}
	endTok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := endTok.(json.Delim)
	if !ok || d != '}' {
		return fmt.Errorf("expected '}', got %v", endTok)
	}
	return nil
}

// foldJSONKey uses the same Unicode simple-fold equivalence as encoding/json.
func foldJSONKey(key string) string {
	runes := []rune(key)
	for i, r := range runes {
		minimum := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		runes[i] = minimum
	}
	return string(runes)
}

func checkArray(dec *json.Decoder) error {
	for dec.More() {
		vtok, err := dec.Token()
		if err != nil {
			return err
		}
		if err := checkTokenValue(dec, vtok); err != nil {
			return err
		}
	}
	endTok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := endTok.(json.Delim)
	if !ok || d != ']' {
		return fmt.Errorf("expected ']', got %v", endTok)
	}
	return nil
}

// DecodeStrict decodes data into target after validating control record size,
// JSON syntax, duplicate keys, trailing bytes, and forbidding unknown fields.
func DecodeStrict(data []byte, target any) error {
	if err := ValidateJSONStructure(data); err != nil {
		return err
	}
	if err := validateRequiredFields(data, target); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("decoding strict json: %w", err)
	}
	return nil
}

// ReadBounded reads up to maxBytes from r. If r contains more than maxBytes, it returns an error.
func ReadBounded(r io.Reader, maxBytes int64) ([]byte, error) {
	lr := io.LimitReader(r, maxBytes+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("reading bounded stream: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrControlRecordTooBig
	}
	return data, nil
}

// ReadControlRecord reads a control record from r bounded by MaxControlRecordSize.
func ReadControlRecord(r io.Reader) ([]byte, error) {
	return ReadBounded(r, MaxControlRecordSize)
}
