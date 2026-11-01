package contract

import (
	"encoding/hex"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// validateDefinitionEncoding checks the string representation before Go's JSON
// decoder can replace malformed input with U+FFFD. It only scans string escapes;
// JSON grammar, fields, duplicates and structure remain owned by DecodeStrict.
func validateDefinitionEncoding(data []byte) error {
	if len(data) > task.MaxControlRecordSize {
		return task.ErrControlRecordTooBig
	}
	if !utf8.Valid(data) {
		return errors.New("definition is not valid UTF-8")
	}
	quoted := false
	for offset := 0; offset < len(data); {
		switch {
		case data[offset] == '"':
			quoted = !quoted
			offset++
		case !quoted || data[offset] != '\\':
			offset++
		case offset+1 < len(data) && data[offset+1] == 'u':
			next, err := definitionUnicodeEscapeEnd(data, offset)
			if err != nil {
				return err
			}
			offset = next
		default:
			// Consume an escaped backslash/quote as one escape. A literal
			// backslash-u must never become a Unicode escape in this scan.
			offset += 2
		}
	}
	return nil
}

func definitionUnicodeEscapeEnd(data []byte, offset int) (int, error) {
	unit, err := definitionUnicodeUnit(data, offset+2)
	if err != nil {
		return 0, err
	}
	next := offset + 6
	if unit >= 0xdc00 && unit <= 0xdfff {
		return 0, fmt.Errorf("unpaired low UTF-16 surrogate at byte %d", offset)
	}
	if unit < 0xd800 || unit > 0xdbff {
		return next, nil
	}
	// A high surrogate must be followed immediately by an escaped low
	// surrogate in this same JSON string, with no intervening escape or text.
	if next+6 > len(data) || data[next] != '\\' || data[next+1] != 'u' {
		return 0, fmt.Errorf("unpaired high UTF-16 surrogate at byte %d", offset)
	}
	low, err := definitionUnicodeUnit(data, next+2)
	if err != nil {
		return 0, err
	}
	if low < 0xdc00 || low > 0xdfff {
		return 0, fmt.Errorf("unpaired high UTF-16 surrogate at byte %d", offset)
	}
	return next + 6, nil
}

func definitionUnicodeUnit(data []byte, offset int) (uint16, error) {
	if offset+4 > len(data) {
		return 0, fmt.Errorf("incomplete JSON Unicode escape at byte %d", offset)
	}
	var decoded [2]byte
	if _, err := hex.Decode(decoded[:], data[offset:offset+4]); err != nil {
		return 0, fmt.Errorf("JSON Unicode escape at byte %d: %w", offset, err)
	}
	return uint16(decoded[0])<<8 | uint16(decoded[1]), nil
}
