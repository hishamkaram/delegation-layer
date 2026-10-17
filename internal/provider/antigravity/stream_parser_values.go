package antigravity

import (
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

func (p *jsonParser) readSmallString(limit int) (string, error) {
	sink := &stringSink{limit: limit}
	if err := p.readJSONString(sink.emit); err != nil {
		return "", err
	}
	return sink.String(), nil
}

func (p *jsonParser) readNullableNumber() (*string, error) {
	b, err := p.nextNonWhitespace()
	if err != nil {
		return nil, p.parseReadError(err)
	}
	if b == 'n' {
		if nullErr := p.readNullAfterFirst(); nullErr != nil {
			return nil, nullErr
		}
		return nil, nil
	}
	if unreadErr := p.cursor.unreadByte(b); unreadErr != nil {
		return nil, unreadErr
	}
	number, err := p.readNumber()
	if err != nil {
		return nil, err
	}
	if !validDurationNumber(number) {
		return nil, &semanticError{reason: "invalid duration_seconds"}
	}
	return &number, nil
}

func (p *jsonParser) readNullableInteger() (*int64, error) {
	b, err := p.nextNonWhitespace()
	if err != nil {
		return nil, p.parseReadError(err)
	}
	if b == 'n' {
		if nullErr := p.readNullAfterFirst(); nullErr != nil {
			return nil, nullErr
		}
		return nil, nil
	}
	if unreadErr := p.cursor.unreadByte(b); unreadErr != nil {
		return nil, unreadErr
	}
	number, err := p.readNumber()
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(number, ".eE") {
		return nil, &semanticError{reason: "integer field is not integral"}
	}
	value, err := strconv.ParseInt(number, 10, 64)
	if err != nil || value < 0 {
		return nil, &semanticError{reason: "integer field is out of range"}
	}
	return &value, nil
}

func (p *jsonParser) readNumber() (string, error) {
	var b strings.Builder
	appendByte := func(value byte) error {
		if b.Len() >= maxNumberBytes {
			return &semanticError{reason: "number exceeds bounded size"}
		}
		return b.WriteByte(value)
	}
	value, err := p.cursor.readByte()
	if err != nil {
		return "", p.parseReadError(err)
	}
	if value == '-' {
		if appendErr := appendByte(value); appendErr != nil {
			return "", appendErr
		}
		value, err = p.cursor.readByte()
		if err != nil {
			return "", p.parseReadError(err)
		}
	}
	delimiter, err := p.readNumberInteger(value, appendByte)
	if errors.Is(err, io.EOF) {
		return b.String(), nil
	}
	if err != nil {
		return "", err
	}
	return p.readNumberRemainder(&b, delimiter, appendByte)
}

func (p *jsonParser) readNumberRemainder(b *strings.Builder, delimiter byte, appendByte func(byte) error) (string, error) {
	if delimiter == '.' {
		fractionDelimiter, err := p.readNumberFraction(appendByte)
		if errors.Is(err, io.EOF) {
			return b.String(), nil
		}
		if err != nil {
			return "", err
		}
		delimiter = fractionDelimiter
	}
	if delimiter == 'e' || delimiter == 'E' {
		exponentDelimiter, err := p.readNumberExponent(delimiter, appendByte)
		if errors.Is(err, io.EOF) {
			return b.String(), nil
		}
		if err != nil {
			return "", err
		}
		delimiter = exponentDelimiter
	}
	if !isValueDelimiter(delimiter) {
		return "", &semanticError{reason: "invalid number delimiter"}
	}
	if err := p.cursor.unreadByte(delimiter); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (p *jsonParser) readNumberInteger(first byte, appendByte func(byte) error) (byte, error) {
	if first == '0' {
		if appendErr := appendByte(first); appendErr != nil {
			return 0, appendErr
		}
		value, err := p.cursor.readByte()
		if err != nil {
			return 0, err
		}
		if value >= '0' && value <= '9' {
			return 0, &semanticError{reason: "number has leading zero"}
		}
		return value, nil
	}
	if first < '1' || first > '9' {
		return 0, &semanticError{reason: "invalid number"}
	}
	if appendErr := appendByte(first); appendErr != nil {
		return 0, appendErr
	}
	for {
		value, err := p.cursor.readByte()
		if err != nil {
			return 0, err
		}
		if value < '0' || value > '9' {
			return value, nil
		}
		if appendErr := appendByte(value); appendErr != nil {
			return 0, appendErr
		}
	}
}

func (p *jsonParser) readNumberFraction(appendByte func(byte) error) (byte, error) {
	if appendErr := appendByte('.'); appendErr != nil {
		return 0, appendErr
	}
	value, err := p.cursor.readByte()
	if err != nil {
		return 0, p.parseReadError(err)
	}
	if value < '0' || value > '9' {
		return 0, &semanticError{reason: "fraction has no digits"}
	}
	if appendErr := appendByte(value); appendErr != nil {
		return 0, appendErr
	}
	for {
		value, err = p.cursor.readByte()
		if err != nil {
			return 0, err
		}
		if value < '0' || value > '9' {
			return value, nil
		}
		if appendErr := appendByte(value); appendErr != nil {
			return 0, appendErr
		}
	}
}

func (p *jsonParser) readNumberExponent(marker byte, appendByte func(byte) error) (byte, error) {
	if appendErr := appendByte(marker); appendErr != nil {
		return 0, appendErr
	}
	value, err := p.cursor.readByte()
	if err != nil {
		return 0, p.parseReadError(err)
	}
	if value == '+' || value == '-' {
		if appendErr := appendByte(value); appendErr != nil {
			return 0, appendErr
		}
		value, err = p.cursor.readByte()
		if err != nil {
			return 0, p.parseReadError(err)
		}
	}
	if value < '0' || value > '9' {
		return 0, &semanticError{reason: "exponent has no digits"}
	}
	if appendErr := appendByte(value); appendErr != nil {
		return 0, appendErr
	}
	for {
		value, err = p.cursor.readByte()
		if err != nil {
			return 0, err
		}
		if value < '0' || value > '9' {
			return value, nil
		}
		if appendErr := appendByte(value); appendErr != nil {
			return 0, appendErr
		}
	}
}

func validDurationNumber(number string) bool {
	if len(number) == 0 || len(number) > maxNumberBytes {
		return false
	}
	if strings.HasPrefix(number, "-") && durationNumberHasNonzeroMagnitude(number) {
		return false
	}
	value, err := strconv.ParseFloat(number, 64)
	return err == nil && value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func durationNumberHasNonzeroMagnitude(number string) bool {
	mantissa := strings.TrimPrefix(number, "-")
	if exponent := strings.IndexAny(mantissa, "eE"); exponent >= 0 {
		mantissa = mantissa[:exponent]
	}
	for _, digit := range mantissa {
		if digit != '0' && digit != '.' {
			return true
		}
	}
	return false
}

func (p *jsonParser) readJSONString(emit func([]byte, rune) error) error {
	b, err := p.nextNonWhitespace()
	if err != nil {
		return p.parseReadError(err)
	}
	if b != '"' {
		return &semanticError{reason: "value is not a string"}
	}
	for {
		b, err = p.cursor.readByte()
		if err != nil {
			return p.parseReadError(err)
		}
		if b == '"' {
			return nil
		}
		if b == '\\' {
			r, decodeErr := p.readEscape()
			if decodeErr != nil {
				return decodeErr
			}
			if emitErr := emitRune(emit, r); emitErr != nil {
				return emitErr
			}
			continue
		}
		if rawErr := p.emitRawStringByte(emit, b); rawErr != nil {
			return rawErr
		}
	}
}

func emitRune(emit func([]byte, rune) error, r rune) error {
	var encoded [utf8.UTFMax]byte
	n := utf8.EncodeRune(encoded[:], r)
	err := emit(encoded[:n], r)
	if err != nil && !errors.Is(err, errWriterFailureRecorded) {
		return err
	}
	return nil
}

func (p *jsonParser) emitRawStringByte(emit func([]byte, rune) error, b byte) error {
	if b < 0x20 {
		return &semanticError{reason: "unescaped control byte"}
	}
	if b < utf8.RuneSelf {
		return emitRune(emit, rune(b))
	}
	runeSize := utf8SequenceSize(b)
	if runeSize == 0 {
		return &semanticError{reason: "invalid UTF-8 leading byte"}
	}
	var sequence [utf8.UTFMax]byte
	sequence[0] = b
	for i := 1; i < runeSize; i++ {
		part, err := p.cursor.readByte()
		if err != nil {
			return p.parseReadError(err)
		}
		if part&0xc0 != 0x80 {
			return &semanticError{reason: "invalid UTF-8 continuation"}
		}
		sequence[i] = part
	}
	if !utf8.Valid(sequence[:runeSize]) {
		return &semanticError{reason: "invalid UTF-8 string"}
	}
	r, decodedSize := utf8.DecodeRune(sequence[:runeSize])
	if decodedSize != runeSize {
		return &semanticError{reason: "invalid UTF-8 sequence"}
	}
	return emitRune(emit, r)
}

func (p *jsonParser) readEscape() (rune, error) {
	b, err := p.cursor.readByte()
	if err != nil {
		return 0, p.parseReadError(err)
	}
	switch b {
	case '"', '\\', '/':
		return rune(b), nil
	case 'b':
		return '\b', nil
	case 'f':
		return '\f', nil
	case 'n':
		return '\n', nil
	case 'r':
		return '\r', nil
	case 't':
		return '\t', nil
	case 'u':
		return p.readUnicodeEscape()
	default:
		return 0, &semanticError{reason: "invalid JSON escape"}
	}
}

func (p *jsonParser) readUnicodeEscape() (rune, error) {
	code, err := p.readHex4()
	if err != nil {
		return 0, err
	}
	if code >= 0xd800 && code <= 0xdbff {
		if err := p.expect('\\'); err != nil {
			return 0, &semanticError{reason: "unpaired high surrogate"}
		}
		if err := p.expect('u'); err != nil {
			return 0, &semanticError{reason: "unpaired high surrogate"}
		}
		low, lowErr := p.readHex4()
		if lowErr != nil || low < 0xdc00 || low > 0xdfff {
			return 0, &semanticError{reason: "invalid surrogate pair"}
		}
		return rune(0x10000 + ((code - 0xd800) << 10) + (low - 0xdc00)), nil
	}
	if code >= 0xdc00 && code <= 0xdfff {
		return 0, &semanticError{reason: "unpaired low surrogate"}
	}
	return rune(code), nil
}

func (p *jsonParser) readHex4() (int, error) {
	value := 0
	for i := 0; i < 4; i++ {
		b, err := p.cursor.readByte()
		if err != nil {
			return 0, p.parseReadError(err)
		}
		digit, ok := hexDigit(b)
		if !ok {
			return 0, &semanticError{reason: "invalid Unicode escape"}
		}
		value = value<<4 | digit
	}
	return value, nil
}

func (p *jsonParser) readNullAfterFirst() error {
	for _, want := range []byte{'u', 'l', 'l'} {
		b, err := p.cursor.readByte()
		if err != nil {
			return p.parseReadError(err)
		}
		if b != want {
			return &semanticError{reason: "invalid null"}
		}
	}
	return nil
}

func (p *jsonParser) expect(want byte) error {
	b, err := p.cursor.readByte()
	if err != nil {
		return p.parseReadError(err)
	}
	if b != want {
		return &semanticError{reason: "unexpected JSON byte"}
	}
	return nil
}

func (p *jsonParser) expectObjectColon() error {
	b, err := p.nextNonWhitespace()
	if err != nil {
		return p.parseReadError(err)
	}
	if b != ':' {
		return &semanticError{reason: "unexpected JSON byte"}
	}
	return nil
}

func (p *jsonParser) nextNonWhitespace() (byte, error) {
	for {
		b, err := p.cursor.readByte()
		if err != nil {
			return 0, err
		}
		if !isJSONWhitespace(b) {
			return b, nil
		}
	}
}

func (p *jsonParser) requiredFields() error {
	if !p.env.statusSeen || !p.env.conversationSeen || !p.env.responseSeen {
		return &semanticError{reason: "required field missing"}
	}
	return nil
}

func (p *jsonParser) parseReadError(err error) error {
	if errors.Is(err, io.EOF) {
		return &semanticError{reason: "truncated envelope"}
	}
	return err
}

func (p *jsonParser) parseFieldError(err error) error {
	var semantic *semanticError
	if errors.As(err, &semantic) {
		return err
	}
	if errors.Is(err, io.EOF) {
		return &semanticError{reason: "truncated envelope"}
	}
	return err
}

func isJSONWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

func isValueDelimiter(b byte) bool {
	return isJSONWhitespace(b) || b == ',' || b == '}' || b == ']'
}

func utf8SequenceSize(first byte) int {
	switch {
	case first >= 0xc2 && first <= 0xdf:
		return 2
	case first >= 0xe0 && first <= 0xef:
		return 3
	case first >= 0xf0 && first <= 0xf4:
		return 4
	default:
		return 0
	}
}

func hexDigit(b byte) (int, bool) {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0'), true
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10, true
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10, true
	default:
		return 0, false
	}
}
