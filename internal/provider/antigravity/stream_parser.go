package antigravity

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	readBufferSize      = 32 * 1024
	maxSmallFieldBytes  = 4 * 1024
	maxDiagnosticBytes  = 64 * 1024
	maxNumberBytes      = 128
	responseBufferBytes = 32 * 1024
)

type semanticError struct{ reason string }

func (e *semanticError) Error() string { return "invalid antigravity envelope: " + e.reason }

var errWriterFailureRecorded = errors.New("antigravity response writer failure recorded")

type usageRecord struct {
	InputTokens     *int64
	OutputTokens    *int64
	ThinkingTokens  *int64
	TotalTokens     *int64
	CacheReadTokens *int64
	NumTurns        *int64
	DurationSeconds *string
}

type envelope struct {
	status           string
	statusSeen       bool
	conversationID   string
	conversationSeen bool
	responseSeen     bool
	responseNonWS    bool
	errorSeen        bool
	errorText        string
	durationSeconds  *string
	numTurns         *int64
	usage            *usageRecord
}

type stdoutResult struct {
	envelope  envelope
	semantic  error
	operation error
}

type byteCursor struct {
	r          io.Reader
	buf        []byte
	pos        int
	end        int
	err        error
	noProgress int
	unread     byte
	hasUnread  bool
}

func newByteCursor(r io.Reader) *byteCursor {
	return &byteCursor{r: r, buf: make([]byte, readBufferSize)}
}

func (c *byteCursor) readByte() (byte, error) {
	if c.hasUnread {
		c.hasUnread = false
		return c.unread, nil
	}
	if c.pos < c.end {
		b := c.buf[c.pos]
		c.pos++
		return b, nil
	}
	if c.err != nil {
		return 0, c.err
	}
	if c.r == nil {
		c.err = errors.New("nil antigravity evidence reader")
		return 0, c.err
	}
	for {
		n, err := c.r.Read(c.buf)
		if n < 0 || n > len(c.buf) {
			c.err = fmt.Errorf("reader returned invalid byte count %d", n)
			return 0, c.err
		}
		if n > 0 {
			c.pos, c.end, c.noProgress = 1, n, 0
			c.err = err
			return c.buf[0], nil
		}
		if err != nil {
			c.err = err
			return 0, err
		}
		c.noProgress++
		if c.noProgress >= 100 {
			c.err = io.ErrNoProgress
			return 0, c.err
		}
	}
}

func (c *byteCursor) unreadByte(b byte) error {
	if c.hasUnread {
		return errors.New("parser lookahead buffer already occupied")
	}
	c.unread, c.hasUnread = b, true
	return nil
}

func (c *byteCursor) drain() error {
	c.hasUnread = false
	c.pos, c.end = 0, 0
	if c.err != nil {
		if errors.Is(c.err, io.EOF) {
			return nil
		}
		return c.err
	}
	buf := make([]byte, readBufferSize)
	noProgress := 0
	for {
		n, err := c.r.Read(buf)
		if n < 0 || n > len(buf) {
			return fmt.Errorf("reader returned invalid byte count %d", n)
		}
		if n > 0 {
			noProgress = 0
		} else if err == nil {
			noProgress++
			if noProgress >= 100 {
				return io.ErrNoProgress
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

type responseSink struct {
	out       io.Writer
	buffer    []byte
	writerErr error
	nonWS     bool
}

func newResponseSink(out io.Writer) *responseSink {
	return &responseSink{out: out, buffer: make([]byte, 0, responseBufferBytes)}
}

func (s *responseSink) emit(data []byte, r rune) error {
	if !unicode.IsSpace(r) {
		s.nonWS = true
	}
	if s.writerErr != nil {
		return errWriterFailureRecorded
	}
	if s.out == nil {
		s.writerErr = errors.New("nil antigravity answer sink")
		return errWriterFailureRecorded
	}
	if len(s.buffer) > 0 && len(s.buffer)+len(data) > cap(s.buffer) {
		s.flush()
		if s.writerErr != nil {
			return errWriterFailureRecorded
		}
	}
	s.buffer = append(s.buffer, data...)
	if len(s.buffer) == cap(s.buffer) {
		s.flush()
	}
	return nil
}

func (s *responseSink) flush() {
	if s.writerErr != nil || len(s.buffer) == 0 {
		return
	}
	pending := s.buffer
	for len(pending) > 0 {
		n, err := s.out.Write(pending)
		if n < 0 || n > len(pending) {
			s.writerErr = fmt.Errorf("answer writer returned invalid byte count %d", n)
			s.buffer = pending
			return
		}
		if n > 0 {
			pending = pending[n:]
		}
		if err != nil {
			s.writerErr = err
			s.buffer = pending
			return
		}
		if n == 0 {
			s.writerErr = io.ErrShortWrite
			s.buffer = pending
			return
		}
	}
	s.buffer = s.buffer[:0]
}

type stringSink struct {
	b     strings.Builder
	limit int
}

func (s *stringSink) emit(data []byte, _ rune) error {
	if s.b.Len()+len(data) > s.limit {
		return &semanticError{reason: "field exceeds bounded size"}
	}
	n, err := s.b.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func (s *stringSink) String() string { return s.b.String() }

type jsonParser struct {
	cursor *byteCursor
	sink   *responseSink
	env    envelope
}

func newJSONParser(r io.Reader, out io.Writer) *jsonParser {
	return &jsonParser{cursor: newByteCursor(r), sink: newResponseSink(out)}
}

func parseStdout(r io.Reader, out io.Writer) stdoutResult {
	p := newJSONParser(r, out)
	err := p.parseEnvelope()
	if err != nil {
		var semantic *semanticError
		if errors.As(err, &semantic) {
			drainErr := p.cursor.drain()
			p.sink.flush()
			return stdoutResult{envelope: p.env, semantic: err, operation: errors.Join(p.sink.writerErr, drainErr)}
		}
		return stdoutResult{envelope: p.env, operation: errors.Join(err, p.sink.writerErr)}
	}
	p.sink.flush()
	return stdoutResult{envelope: p.env, operation: p.sink.writerErr}
}

func (p *jsonParser) parseEnvelope() error {
	b, err := p.nextNonWhitespace()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return &semanticError{reason: "missing envelope"}
		}
		return err
	}
	if b != '{' {
		return &semanticError{reason: "top-level value is not an object"}
	}
	if parseErr := p.parseTopObject(); parseErr != nil {
		return parseErr
	}
	for {
		b, err = p.cursor.readByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return p.requiredFields()
			}
			return err
		}
		if isJSONWhitespace(b) {
			continue
		}
		return &semanticError{reason: "trailing data"}
	}
}

func (p *jsonParser) parseTopObject() error {
	seen := make(map[string]struct{}, 7)
	b, err := p.nextNonWhitespace()
	if err != nil {
		return p.parseReadError(err)
	}
	if b == '}' {
		return p.requiredFields()
	}
	if unreadErr := p.cursor.unreadByte(b); unreadErr != nil {
		return unreadErr
	}
	for {
		key, err := p.readSmallString(maxSmallFieldBytes)
		if err != nil {
			return p.parseFieldError(err)
		}
		if _, exists := seen[key]; exists {
			return &semanticError{reason: "duplicate envelope key"}
		}
		seen[key] = struct{}{}
		if expectErr := p.expectObjectColon(); expectErr != nil {
			return expectErr
		}
		if fieldErr := p.parseTopField(key); fieldErr != nil {
			return fieldErr
		}
		done, err := p.finishObjectEntry()
		if err != nil {
			return err
		}
		if done {
			return p.requiredFields()
		}
	}
}

func (p *jsonParser) finishObjectEntry() (bool, error) {
	b, err := p.nextNonWhitespace()
	if err != nil {
		return false, p.parseReadError(err)
	}
	if b == '}' {
		return true, nil
	}
	if b != ',' {
		return false, &semanticError{reason: "object separator missing"}
	}
	b, err = p.nextNonWhitespace()
	if err != nil {
		return false, p.parseReadError(err)
	}
	if b == '}' {
		return false, &semanticError{reason: "trailing comma"}
	}
	if err := p.cursor.unreadByte(b); err != nil {
		return false, err
	}
	return false, nil
}

func (p *jsonParser) parseTopField(key string) error {
	switch key {
	case "status":
		value, err := p.readSmallString(maxSmallFieldBytes)
		if err != nil {
			return p.parseFieldError(err)
		}
		p.env.status, p.env.statusSeen = value, true
	case "conversation_id":
		value, err := p.readSmallString(maxSmallFieldBytes)
		if err != nil {
			return p.parseFieldError(err)
		}
		p.env.conversationID, p.env.conversationSeen = value, true
	case "response":
		p.env.responseSeen = true
		if err := p.readJSONString(p.sink.emit); err != nil {
			return p.parseFieldError(err)
		}
		p.env.responseNonWS = p.sink.nonWS
	case "error":
		value, err := p.readSmallString(maxDiagnosticBytes)
		if err != nil {
			return p.parseFieldError(err)
		}
		p.env.errorText, p.env.errorSeen = value, true
	case "duration_seconds":
		value, err := p.readNullableNumber()
		if err != nil {
			return p.parseFieldError(err)
		}
		p.env.durationSeconds = value
	case "num_turns":
		value, err := p.readNullableInteger()
		if err != nil {
			return p.parseFieldError(err)
		}
		p.env.numTurns = value
	case "usage":
		value, err := p.readUsage()
		if err != nil {
			return p.parseFieldError(err)
		}
		p.env.usage = value
	default:
		return &semanticError{reason: "unknown envelope key"}
	}
	return nil
}

func (p *jsonParser) readUsage() (*usageRecord, error) {
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
	if b != '{' {
		return nil, &semanticError{reason: "usage is not an object or null"}
	}
	return p.readUsageObject()
}

func (p *jsonParser) readUsageObject() (*usageRecord, error) {
	usage := &usageRecord{}
	seen := make(map[string]struct{}, 5)
	b, err := p.nextNonWhitespace()
	if err != nil {
		return nil, p.parseReadError(err)
	}
	if b == '}' {
		return usage, nil
	}
	if unreadErr := p.cursor.unreadByte(b); unreadErr != nil {
		return nil, unreadErr
	}
	for {
		key, err := p.readSmallString(maxSmallFieldBytes)
		if err != nil {
			return nil, p.parseFieldError(err)
		}
		if _, exists := seen[key]; exists {
			return nil, &semanticError{reason: "duplicate usage key"}
		}
		seen[key] = struct{}{}
		if counterErr := p.readUsageCounter(usage, key); counterErr != nil {
			return nil, counterErr
		}
		done, err := p.finishObjectEntry()
		if err != nil {
			return nil, err
		}
		if done {
			return usage, nil
		}
	}
}

func (p *jsonParser) readUsageCounter(usage *usageRecord, key string) error {
	if expectErr := p.expectObjectColon(); expectErr != nil {
		return expectErr
	}
	counter, err := p.readNullableInteger()
	if err != nil {
		return p.parseFieldError(err)
	}
	switch key {
	case "input_tokens":
		usage.InputTokens = counter
	case "output_tokens":
		usage.OutputTokens = counter
	case "thinking_tokens":
		usage.ThinkingTokens = counter
	case "cache_read_tokens":
		usage.CacheReadTokens = counter
	case "total_tokens":
		usage.TotalTokens = counter
	default:
		return &semanticError{reason: "unknown usage key"}
	}
	return nil
}

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

type timeoutMatcher struct {
	tail  []byte
	found bool
}

func (m *timeoutMatcher) feed(chunk []byte) {
	if m.found {
		return
	}
	combined := make([]byte, 0, len(m.tail)+len(chunk))
	combined = append(combined, m.tail...)
	combined = append(combined, chunk...)
	if bytes.Contains(combined, []byte(TimeoutMarker)) {
		m.found = true
		return
	}
	keep := len(TimeoutMarker) - 1
	if len(combined) > keep {
		combined = combined[len(combined)-keep:]
	}
	m.tail = append(m.tail[:0], combined...)
}

func scanTimeout(reader io.Reader) (bool, error) {
	if reader == nil {
		return false, errors.New("nil antigravity stderr reader")
	}
	matcher := &timeoutMatcher{}
	buf := make([]byte, readBufferSize)
	noProgress := 0
	for {
		n, err := reader.Read(buf)
		if n < 0 || n > len(buf) {
			return false, fmt.Errorf("stderr reader returned invalid byte count %d", n)
		}
		if n > 0 {
			noProgress = 0
			matcher.feed(buf[:n])
		} else if err == nil {
			noProgress++
			if noProgress >= 100 {
				return false, io.ErrNoProgress
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return matcher.found, nil
			}
			return false, err
		}
	}
}
