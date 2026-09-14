package antigravity

import (
	"errors"
	"sync"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type identityObserver struct {
	mu       sync.Mutex
	parser   identityScanner
	record   func(task.SessionIdentity) error
	callback error
}

// NewIdentityObserver builds a bounded observer for the agy envelope's exact
// conversation_id field. It records at most one valid identity and never
// selects a most-recent or provider-database session.
func NewIdentityObserver(taskID string, expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
	if err := task.ValidateTaskID(taskID); err != nil {
		return nil, err
	}
	if expected.Required && !validConversationID(expected.ID) {
		return nil, errors.New("required antigravity conversation ID is invalid")
	}
	if !expected.Required && expected.ID != "" {
		return nil, errors.New("unexpected antigravity continuation ID")
	}
	if record == nil {
		return nil, errors.New("nil antigravity identity recorder")
	}
	o := &identityObserver{record: record}
	o.parser = identityScanner{expected: expected, onIdentity: o.identity}
	return o, nil
}

func (o *identityObserver) Observe(data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.parser.feed(data)
}

func (o *identityObserver) Complete() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.parser.finish()
	return o.callback
}

func (o *identityObserver) identity(id string) {
	if o.callback != nil {
		return
	}
	o.callback = o.record(task.SessionIdentity{Provider: Provider, ConversationID: id})
}

type identityScanner struct {
	expected              task.SessionExpectation
	onIdentity            func(string)
	started               bool
	depth                 int
	expectingKey          bool
	expectingColon        bool
	wantConversationValue bool
	inString              bool
	escaped               bool
	stringKind            identityStringKind
	stringBytes           []byte
	stringEscape          identityEscapeState
	unicodeValue          int
	unicodeDigits         int
	pendingHighSurrogate  int
	rawUTF8               [utf8.UTFMax]byte
	rawUTF8Len            int
	rawUTF8Need           int
	conversationSeen      bool
	key                   string
	stopped               bool
}

type identityStringKind uint8

const (
	identityStringOther identityStringKind = iota
	identityStringKey
	identityStringConversation
)

type identityEscapeState uint8

const (
	identityEscapeNone identityEscapeState = iota
	identityEscapeAfterSlash
	identityEscapeUnicode
	identityEscapeLowSlash
	identityEscapeLowU
	identityEscapeLowUnicode
)

func (s *identityScanner) feed(data []byte) {
	if s.stopped {
		return
	}
	for _, b := range data {
		if s.stopped {
			return
		}
		if s.inString {
			s.feedStringByte(b)
			continue
		}
		s.feedStructuralByte(b)
	}
}

func (s *identityScanner) feedStringByte(b byte) {
	if s.stringKind == identityStringOther {
		s.feedOtherStringByte(b)
		return
	}
	s.feedDecodedStringByte(b)
}

func (s *identityScanner) feedOtherStringByte(b byte) {
	if s.escaped {
		s.escaped = false
		return
	}
	if b == '\\' {
		s.escaped = true
		return
	}
	if b == '"' {
		s.inString = false
		s.finishString()
	}
}

func (s *identityScanner) feedDecodedStringByte(b byte) {
	if s.rawUTF8Need > 0 {
		s.feedRawUTF8Byte(b)
		return
	}

	switch s.stringEscape {
	case identityEscapeAfterSlash:
		s.feedEscapeByte(b)
	case identityEscapeUnicode, identityEscapeLowUnicode:
		s.feedUnicodeByte(b)
	case identityEscapeLowSlash:
		s.feedLowSlashByte(b)
	case identityEscapeLowU:
		s.feedLowUByte(b)
	case identityEscapeNone:
		s.feedPlainDecodedByte(b)
	default:
		s.stopped = true
	}
}

func (s *identityScanner) feedRawUTF8Byte(b byte) {
	if b&0xc0 != 0x80 {
		s.stopped = true
		return
	}
	s.rawUTF8[s.rawUTF8Len] = b
	s.rawUTF8Len++
	if s.rawUTF8Len != s.rawUTF8Need {
		return
	}
	sequence := s.rawUTF8[:s.rawUTF8Len]
	if !utf8.Valid(sequence) {
		s.stopped = true
		return
	}
	r, size := utf8.DecodeRune(sequence)
	if size != len(sequence) {
		s.stopped = true
		return
	}
	s.rawUTF8Len, s.rawUTF8Need = 0, 0
	s.appendOrStop(r)
}

func (s *identityScanner) feedEscapeByte(b byte) {
	s.stringEscape = identityEscapeNone
	switch b {
	case '"', '\\', '/':
		s.appendOrStop(rune(b))
	case 'b':
		s.appendOrStop('\b')
	case 'f':
		s.appendOrStop('\f')
	case 'n':
		s.appendOrStop('\n')
	case 'r':
		s.appendOrStop('\r')
	case 't':
		s.appendOrStop('\t')
	case 'u':
		s.stringEscape, s.unicodeValue, s.unicodeDigits = identityEscapeUnicode, 0, 0
	default:
		s.stopped = true
	}
}

func (s *identityScanner) feedUnicodeByte(b byte) {
	digit, ok := hexDigit(b)
	if !ok {
		s.stopped = true
		return
	}
	s.unicodeValue = s.unicodeValue<<4 | digit
	s.unicodeDigits++
	if s.unicodeDigits != 4 {
		return
	}
	if s.stringEscape == identityEscapeUnicode {
		s.finishHighUnicode()
	} else {
		s.finishLowUnicode()
	}
	s.unicodeValue, s.unicodeDigits = 0, 0
}

func (s *identityScanner) finishHighUnicode() {
	switch {
	case s.unicodeValue >= 0xd800 && s.unicodeValue <= 0xdbff:
		s.pendingHighSurrogate = s.unicodeValue
		s.stringEscape = identityEscapeLowSlash
	case s.unicodeValue >= 0xdc00 && s.unicodeValue <= 0xdfff:
		s.stopped = true
	default:
		s.appendOrStop(rune(s.unicodeValue))
		s.stringEscape = identityEscapeNone
	}
}

func (s *identityScanner) finishLowUnicode() {
	if s.unicodeValue < 0xdc00 || s.unicodeValue > 0xdfff {
		s.stopped = true
		return
	}
	r := rune(0x10000 + ((s.pendingHighSurrogate - 0xd800) << 10) + (s.unicodeValue - 0xdc00))
	s.appendOrStop(r)
	s.pendingHighSurrogate = -1
	s.stringEscape = identityEscapeNone
}

func (s *identityScanner) feedLowSlashByte(b byte) {
	if b != '\\' {
		s.stopped = true
		return
	}
	s.stringEscape = identityEscapeLowU
}

func (s *identityScanner) feedLowUByte(b byte) {
	if b != 'u' {
		s.stopped = true
		return
	}
	s.stringEscape, s.unicodeValue, s.unicodeDigits = identityEscapeLowUnicode, 0, 0
}

func (s *identityScanner) feedPlainDecodedByte(b byte) {
	if b == '"' {
		s.inString = false
		s.finishString()
		return
	}
	if b == '\\' {
		s.stringEscape = identityEscapeAfterSlash
		return
	}
	if b < 0x20 {
		s.stopped = true
		return
	}
	if b < utf8.RuneSelf {
		if !s.appendDecodedRune(rune(b)) {
			s.stopped = true
		}
		return
	}
	size := utf8SequenceSize(b)
	if size == 0 {
		s.stopped = true
		return
	}
	s.rawUTF8[0], s.rawUTF8Len, s.rawUTF8Need = b, 1, size
}

func (s *identityScanner) appendOrStop(r rune) {
	if !s.appendDecodedRune(r) {
		s.stopped = true
	}
}

func (s *identityScanner) appendDecodedRune(r rune) bool {
	var encoded [utf8.UTFMax]byte
	n := utf8.EncodeRune(encoded[:], r)
	if len(s.stringBytes)+n > maxSmallFieldBytes {
		return false
	}
	s.stringBytes = append(s.stringBytes, encoded[:n]...)
	return true
}

func (s *identityScanner) feedStructuralByte(b byte) {
	if isJSONWhitespace(b) {
		return
	}
	if !s.started {
		s.startDocument(b)
		return
	}
	if s.expectingColon {
		s.feedColon(b)
		return
	}
	if s.wantConversationValue {
		s.feedConversationStart(b)
		return
	}
	if s.expectingKey {
		s.feedKeyStart(b)
		return
	}
	if b == '"' {
		s.beginString(identityStringOther)
		return
	}
	s.feedStructuralValueByte(b)
}

func (s *identityScanner) startDocument(b byte) {
	if b != '{' {
		s.stopped = true
		return
	}
	s.started, s.depth, s.expectingKey = true, 1, true
}

func (s *identityScanner) feedColon(b byte) {
	if b != ':' {
		s.stopped = true
		return
	}
	s.expectingColon = false
	s.wantConversationValue = s.key == "conversation_id"
}

func (s *identityScanner) feedConversationStart(b byte) {
	if b != '"' {
		s.stopped = true
		return
	}
	s.beginString(identityStringConversation)
}

func (s *identityScanner) feedKeyStart(b byte) {
	if b == '}' && s.depth == 1 {
		s.stopped = true
		return
	}
	if b != '"' || s.depth != 1 {
		s.stopped = true
		return
	}
	s.beginString(identityStringKey)
}

func (s *identityScanner) feedStructuralValueByte(b byte) {
	switch b {
	case '{', '[':
		s.depth++
	case '}', ']':
		if s.depth == 0 {
			s.stopped = true
			return
		}
		s.depth--
		if b == '}' && s.depth == 0 {
			s.stopped = true
		}
	case ',':
		if s.depth == 1 {
			s.expectingKey = true
		}
	}
}

func (s *identityScanner) beginString(kind identityStringKind) {
	s.inString, s.stringKind = true, kind
	s.escaped = false
	s.stringBytes = s.stringBytes[:0]
	s.stringEscape = identityEscapeNone
	s.unicodeValue, s.unicodeDigits = 0, 0
	s.pendingHighSurrogate = -1
	s.rawUTF8Len, s.rawUTF8Need = 0, 0
}

func (s *identityScanner) finishString() {
	switch s.stringKind {
	case identityStringKey:
		s.key = string(s.stringBytes)
		s.expectingKey, s.expectingColon = false, true
	case identityStringConversation:
		s.wantConversationValue = false
		if s.conversationSeen {
			s.stopped = true
			break
		}
		s.conversationSeen = true
		if validConversationID(string(s.stringBytes)) && (s.expected.ID == "" || s.expected.ID == string(s.stringBytes)) {
			s.onIdentity(string(s.stringBytes))
		}
	case identityStringOther:
		// Other values are consumed only to preserve structural scanning.
	}
	s.stringKind, s.stringBytes = identityStringOther, s.stringBytes[:0]
}

func (s *identityScanner) finish() { s.stopped = true }

var _ execution.IdentityObserver = (*identityObserver)(nil)
