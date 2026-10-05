// Package providerjson reads selected fields from native JSON evidence without
// retaining opaque payloads. Readers and their lifetimes belong to the caller.
package providerjson

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	BufferSize        = 32 * 1024
	maxDepth          = 10000
	maxUniqueKeyBytes = 4096
)

// FieldError is a syntax or relevant-field error, never a provider payload excerpt.
type FieldError struct {
	Code   string
	Offset int64
	Field  string
}

func (e *FieldError) Error() string { return fmt.Sprintf("%s at byte %d", e.Code, e.Offset) }

// Value is a range in a caller-owned, callback-scoped reader.
type Value struct {
	Reader     io.ReaderAt
	Start, End int64
}

func (v Value) cursor() *cursor {
	return &cursor{r: bufio.NewReaderSize(io.NewSectionReader(v.Reader, v.Start, v.End-v.Start), BufferSize), pos: v.Start}
}

func (v Value) Validate() error {
	if err := v.valid(); err != nil {
		return err
	}
	c := v.cursor()
	if err := c.value(0); err != nil {
		return err
	}
	return c.finish()
}

// ValidateUniqueKeys validates the complete JSON value and rejects duplicate
// object keys, including case-folded aliases, at every nesting level. The
// ordinary Object method intentionally skips duplicate opaque extensions for
// historical predicates; current contracts that require strict key identity
// opt into this separate validation pass.
func (v Value) ValidateUniqueKeys() error {
	if err := v.valid(); err != nil {
		return err
	}
	c := v.cursor()
	if err := c.uniqueValue(0); err != nil {
		return err
	}
	return c.finish()
}

// Object selects only named fields. Unknown members are validated and skipped.
// Duplicate selected names (including case aliases) are ambiguous and rejected.
func (v Value) Object(names ...string) (map[string]Value, error) {
	if err := v.valid(); err != nil {
		return nil, err
	}
	c := v.cursor()
	fields := make(map[string]Value, len(names))
	if err := c.expect('{'); err != nil {
		return nil, err
	}
	if c.peek() == '}' {
		if _, err := c.byte(); err != nil {
			return nil, err
		}
		return fields, c.finish()
	}
	for {
		key, complete, field, err := c.member(v.Reader)
		if err != nil {
			return nil, err
		}
		if complete {
			if err = selectField(fields, names, key, field); err != nil {
				return nil, err
			}
		}
		b, err := c.separator('}')
		if err != nil {
			return nil, err
		}
		if b == '}' {
			break
		}
	}
	return fields, c.finish()
}

func selectField(fields map[string]Value, names []string, key string, field Value) error {
	for _, name := range names {
		if !strings.EqualFold(key, name) {
			continue
		}
		if _, exists := fields[name]; exists || key != name {
			return &FieldError{Code: "duplicate_control_field", Offset: field.Start, Field: name}
		}
		fields[name] = field
	}
	return nil
}

func (c *cursor) member(reader io.ReaderAt) (string, bool, Value, error) {
	var key strings.Builder
	complete := true
	err := c.stringValue(func(p []byte) error {
		if len(p) > 4096-key.Len() {
			complete = false
			return nil
		}
		if !complete {
			return nil
		}
		_, err := key.Write(p)
		return err
	})
	if err != nil {
		return "", false, Value{}, err
	}
	if err = c.expect(':'); err != nil {
		return "", false, Value{}, err
	}
	c.space()
	start := c.pos
	if err = c.value(0); err != nil {
		return "", false, Value{}, err
	}
	return key.String(), complete, Value{Reader: reader, Start: start, End: c.pos}, nil
}

// Array visits one element at a time, retaining no element list.
func (v Value) Array(visit func(Value) error) error {
	if err := v.valid(); err != nil {
		return err
	}
	c := v.cursor()
	if err := c.expect('['); err != nil {
		return err
	}
	if c.peek() == ']' {
		if _, err := c.byte(); err != nil {
			return err
		}
		return c.finish()
	}
	for {
		c.space()
		start := c.pos
		if err := c.value(0); err != nil {
			return err
		}
		if err := visit(Value{Reader: v.Reader, Start: start, End: c.pos}); err != nil {
			return err
		}
		b, err := c.separator(']')
		if err != nil {
			return err
		}
		if b == ']' {
			return c.finish()
		}
	}
}

// String decodes a bounded control field. Answer text uses CopyString instead.
func (v Value) String(limit int) (string, error) {
	var b strings.Builder
	err := v.CopyString(writerFunc(func(p []byte) (int, error) {
		if len(p) > limit-b.Len() {
			return 0, &FieldError{Code: "control_field_too_large", Offset: v.Start}
		}
		return b.Write(p)
	}))
	return b.String(), err
}

// Raw retains a bounded control value; callers must never use it for opaque data.
func (v Value) Raw(limit int) ([]byte, error) {
	if err := v.valid(); err != nil {
		return nil, err
	}
	if v.End-v.Start > int64(limit) {
		return nil, &FieldError{Code: "control_field_too_large", Offset: v.Start}
	}
	b, err := io.ReadAll(io.NewSectionReader(v.Reader, v.Start, v.End-v.Start))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != v.End-v.Start {
		return nil, io.ErrUnexpectedEOF
	}
	return b, nil
}

func (v Value) CopyString(out io.Writer) error {
	if err := v.valid(); err != nil {
		return err
	}
	if out == nil {
		return errors.New("nil native answer writer")
	}
	c := v.cursor()
	if err := c.stringValue(func(p []byte) error {
		n, err := out.Write(p)
		if err == nil && n != len(p) {
			return io.ErrShortWrite
		}
		return err
	}); err != nil {
		return err
	}
	return c.finish()
}

func (v Value) Nonblank() (bool, error) {
	found := false
	err := v.CopyString(writerFunc(func(p []byte) (int, error) {
		size := len(p)
		for len(p) > 0 {
			r, n := utf8.DecodeRune(p)
			if !unicode.IsSpace(r) {
				found = true
			}
			p = p[n:]
		}
		return size, nil
	}))
	return found, err
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

type cursor struct {
	r   *bufio.Reader
	pos int64
}

func (c *cursor) fault() error { return &FieldError{Code: "malformed_json", Offset: c.pos} }
func (c *cursor) byte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.pos++
	}
	if errors.Is(err, io.EOF) {
		return 0, c.fault()
	}
	return b, err
}

func (c *cursor) peek() byte {
	c.space()
	b, err := c.r.Peek(1)
	if err != nil {
		return 0
	}
	return b[0]
}

func (c *cursor) space() {
	for {
		b, err := c.r.Peek(1)
		if err != nil || !strings.ContainsRune(" \t\r\n", rune(b[0])) {
			return
		}
		if _, err = c.byte(); err != nil {
			return
		}
	}
}

func (c *cursor) expect(want byte) error {
	c.space()
	b, err := c.byte()
	if err != nil {
		return err
	}
	if b != want {
		return c.fault()
	}
	return nil
}

func (c *cursor) finish() error {
	c.space()
	_, err := c.r.Peek(1)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.fault()
}

func (c *cursor) separator(end byte) (byte, error) {
	c.space()
	b, err := c.byte()
	if err != nil {
		return 0, err
	}
	if b != end && b != ',' {
		return 0, c.fault()
	}
	return b, nil
}

func (c *cursor) value(depth int) error {
	if depth > maxDepth {
		return &FieldError{Code: "nesting_limit", Offset: c.pos}
	}
	switch c.peek() {
	case '"':
		return c.stringValue(nil)
	case '{', '[':
		return c.container(depth)
	case 't':
		return c.literal("true")
	case 'f':
		return c.literal("false")
	case 'n':
		return c.literal("null")
	default:
		return c.number()
	}
}

func (c *cursor) uniqueValue(depth int) error {
	if depth > maxDepth {
		return &FieldError{Code: "nesting_limit", Offset: c.pos}
	}
	switch c.peek() {
	case '{':
		return c.uniqueObject(depth)
	case '[':
		return c.uniqueArray(depth)
	default:
		return c.value(depth)
	}
}

func (c *cursor) uniqueObject(depth int) error {
	if _, err := c.byte(); err != nil {
		return err
	}
	if c.peek() == '}' {
		_, err := c.byte()
		return err
	}
	keys := make([]string, 0, 8)
	for {
		key, err := c.uniqueObjectKey()
		if err != nil {
			return err
		}
		if uniqueKeyDuplicate(keys, key) {
			return &FieldError{Code: "duplicate_control_field", Offset: c.pos, Field: key}
		}
		keys = append(keys, key)
		if err = c.expect(':'); err != nil {
			return err
		}
		if err = c.uniqueValue(depth + 1); err != nil {
			return err
		}
		b, err := c.separator('}')
		if err != nil {
			return err
		}
		if b == '}' {
			return nil
		}
	}
}

func (c *cursor) uniqueObjectKey() (string, error) {
	var key strings.Builder
	complete := true
	if err := c.stringValue(func(chunk []byte) error {
		if key.Len()+len(chunk) > maxUniqueKeyBytes {
			complete = false
			return nil
		}
		if !complete {
			return nil
		}
		_, err := key.Write(chunk)
		return err
	}); err != nil {
		return "", err
	}
	if !complete {
		return "", &FieldError{Code: "object_key_too_large", Offset: c.pos - int64(key.Len()), Field: key.String()}
	}
	return key.String(), nil
}

func uniqueKeyDuplicate(keys []string, key string) bool {
	for _, previous := range keys {
		if strings.EqualFold(previous, key) {
			return true
		}
	}
	return false
}

func (c *cursor) uniqueArray(depth int) error {
	if _, err := c.byte(); err != nil {
		return err
	}
	if c.peek() == ']' {
		_, err := c.byte()
		return err
	}
	for {
		if err := c.uniqueValue(depth + 1); err != nil {
			return err
		}
		b, err := c.separator(']')
		if err != nil {
			return err
		}
		if b == ']' {
			return nil
		}
	}
}

func (c *cursor) literal(s string) error {
	for i := range s {
		b, err := c.byte()
		if err != nil {
			return err
		}
		if b != s[i] {
			return c.fault()
		}
	}
	return nil
}

func (c *cursor) container(depth int) error {
	opening, err := c.byte()
	if err != nil {
		return err
	}
	end := byte(']')
	if opening == '{' {
		end = '}'
	}
	if c.peek() == end {
		_, err = c.byte()
		return err
	}
	for {
		if opening == '{' {
			if err = c.stringValue(nil); err != nil {
				return err
			}
			if err = c.expect(':'); err != nil {
				return err
			}
		}
		if err = c.value(depth + 1); err != nil {
			return err
		}
		b, e := c.separator(end)
		if e != nil {
			return e
		}
		if b == end {
			return nil
		}
	}
}

func (c *cursor) number() error {
	if c.peek() == '-' {
		if _, err := c.byte(); err != nil {
			return err
		}
	}
	b, err := c.byte()
	if err != nil {
		return err
	}
	if b < '0' || b > '9' {
		return c.fault()
	}
	if b != '0' {
		if err = c.digits(false); err != nil {
			return err
		}
	}
	if b = c.peekRaw(); b == '.' {
		if _, err = c.byte(); err != nil {
			return err
		}
		if err = c.digits(true); err != nil {
			return err
		}
	}
	return c.exponent()
}

func (c *cursor) exponent() error {
	b := c.peekRaw()
	if b != 'e' && b != 'E' {
		return nil
	}
	if _, err := c.byte(); err != nil {
		return err
	}
	if b = c.peekRaw(); b == '+' || b == '-' {
		if _, err := c.byte(); err != nil {
			return err
		}
	}
	return c.digits(true)
}

func (c *cursor) peekRaw() byte {
	b, err := c.r.Peek(1)
	if err != nil {
		return 0
	}
	return b[0]
}

func (c *cursor) digits(required bool) error {
	count := 0
	for {
		b := c.peekRaw()
		if b < '0' || b > '9' {
			break
		}
		if _, err := c.byte(); err != nil {
			return err
		}
		count++
	}
	if required && count == 0 {
		return c.fault()
	}
	return nil
}

func (c *cursor) stringValue(emit func([]byte) error) error {
	if err := c.expect('"'); err != nil {
		return err
	}
	var buffer [BufferSize]byte
	n := 0
	flush := func() error { err := emitStringChunk(emit, buffer[:n]); n = 0; return err }
	emitASCII := func(data []byte) error {
		if err := flush(); err != nil {
			return err
		}
		return emitStringChunk(emit, data)
	}
	for {
		advanced, err := c.asciiChunk(emitASCII)
		if err != nil {
			return err
		}
		if advanced {
			continue
		}
		r, done, err := c.nextStringRune()
		if err != nil {
			return err
		}
		if done {
			return flush()
		}
		if n+utf8.UTFMax > len(buffer) {
			if err = flush(); err != nil {
				return err
			}
		}
		n += utf8.EncodeRune(buffer[n:], r)
	}
}

// asciiChunk consumes a validated ASCII run directly from the bounded reader
// buffer. Opaque strings need no copy, and answer writes stay at BufferSize.
func (c *cursor) asciiChunk(emit func([]byte) error) (bool, error) {
	if _, err := c.r.Peek(1); err != nil {
		if errors.Is(err, io.EOF) {
			return false, c.fault()
		}
		return false, err
	}
	data, err := c.r.Peek(c.r.Buffered())
	if err != nil {
		return false, err
	}
	n := asciiPrefix(data)
	if n == 0 {
		return false, nil
	}
	if err = emitStringChunk(emit, data[:n]); err != nil {
		return false, err
	}
	if _, err = c.r.Discard(n); err != nil {
		return false, err
	}
	c.pos += int64(n)
	return true, nil
}

func asciiPrefix(data []byte) int {
	for i, b := range data {
		if b < 0x20 || b >= utf8.RuneSelf || b == '"' || b == '\\' {
			return i
		}
	}
	return len(data)
}

func (c *cursor) nextStringRune() (rune, bool, error) {
	b, err := c.byte()
	if err != nil {
		return 0, false, err
	}
	if b == '"' {
		return 0, true, nil
	}
	if b < 0x20 {
		return 0, false, c.fault()
	}
	r, err := c.stringRune(b)
	return r, false, err
}

func emitStringChunk(emit func([]byte) error, data []byte) error {
	if emit == nil || len(data) == 0 {
		return nil
	}
	return emit(data)
}

func (c *cursor) stringRune(b byte) (rune, error) {
	if b == '\\' {
		return c.escape()
	}
	if b < utf8.RuneSelf {
		return rune(b), nil
	}
	return c.rune(b)
}

func (c *cursor) rune(first byte) (rune, error) {
	var raw [utf8.UTFMax]byte
	raw[0] = first
	n := 1
	for !utf8.FullRune(raw[:n]) && n < len(raw) {
		b, err := c.byte()
		if err != nil {
			return 0, err
		}
		raw[n] = b
		n++
	}
	r, size := utf8.DecodeRune(raw[:n])
	if r == utf8.RuneError && size == 1 {
		return 0, c.fault()
	}
	return r, nil
}

func (c *cursor) escape() (rune, error) {
	b, err := c.byte()
	if err != nil {
		return 0, err
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
		return c.unicode()
	default:
		return 0, c.fault()
	}
}

func (c *cursor) hex() (rune, error) {
	var r rune
	for range 4 {
		b, err := c.byte()
		if err != nil {
			return 0, err
		}
		r *= 16
		switch {
		case b >= '0' && b <= '9':
			r += rune(b - '0')
		case b >= 'a' && b <= 'f':
			r += rune(b - 'a' + 10)
		case b >= 'A' && b <= 'F':
			r += rune(b - 'A' + 10)
		default:
			return 0, c.fault()
		}
	}
	return r, nil
}

func (c *cursor) unicode() (rune, error) {
	r, err := c.hex()
	if err != nil {
		return 0, err
	}
	if r >= 0xdc00 && r <= 0xdfff {
		return 0, c.fault()
	}
	if r < 0xd800 || r > 0xdbff {
		return r, nil
	}
	if err = c.literal(`\u`); err != nil {
		return 0, err
	}
	low, err := c.hex()
	if err != nil {
		return 0, err
	}
	if low < 0xdc00 || low > 0xdfff {
		return 0, c.fault()
	}
	return 0x10000 + (r-0xd800)*0x400 + (low - 0xdc00), nil
}

func (v Value) valid() error {
	if v.Reader == nil || v.Start < 0 || v.End < v.Start {
		return &FieldError{Code: "missing_or_invalid_value", Offset: max(v.Start, 0)}
	}
	return nil
}
