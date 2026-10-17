package antigravity

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
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
	status                    string
	statusSeen                bool
	conversationID            string
	conversationSeen          bool
	responseSeen              bool
	responseEmpty             bool
	responseNonWS             bool
	errorSeen                 bool
	errorText                 string
	authenticationUnavailable bool
	durationSeconds           *string
	numTurns                  *int64
	usage                     *usageRecord
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
	hasData   bool
	nonWS     bool
}

func newResponseSink(out io.Writer) *responseSink {
	return &responseSink{out: out, buffer: make([]byte, 0, responseBufferBytes)}
}

func (s *responseSink) emit(data []byte, r rune) error {
	s.hasData = true
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
		p.env.responseEmpty = !p.sink.hasData
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
