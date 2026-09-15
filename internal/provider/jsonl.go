package provider

import (
	"errors"
	"fmt"
	"io"
)

const jsonlReadBufferBytes = 32 * 1024

var (
	// ErrJSONLLineTooLong identifies a semantic line-bound violation. The
	// framer stops retaining state after this error while ReadJSONL continues
	// consuming the reader so the caller can still reach EOF.
	ErrJSONLLineTooLong  = errors.New("JSONL line exceeds configured maximum")
	ErrJSONLInvalidLimit = errors.New("JSONL maximum line size must be positive")
)

// JSONLFramer performs bounded byte framing for provider-owned JSONL
// semantics. The callback receives one complete line without its newline and
// may return a semantic error; neither that error nor an overlong line stops
// the enclosing reader loop from draining to EOF.
type JSONLFramer struct {
	maxBytes int
	line     []byte
	callback func([]byte) error
	semantic error
	done     bool
}

// NewJSONLFramer creates a bounded framer. It has no knowledge of event names,
// schemas, or terminal states; those remain in each adapter's callback.
func NewJSONLFramer(maxBytes int, callback func([]byte) error) *JSONLFramer {
	framer := &JSONLFramer{maxBytes: maxBytes, callback: callback}
	if maxBytes <= 0 {
		framer.semantic = ErrJSONLInvalidLimit
	}
	return framer
}

// Feed supplies another capture chunk. After a semantic error it intentionally
// discards bytes while retaining no additional parser state.
func (f *JSONLFramer) Feed(data []byte) {
	if f == nil || f.done || f.semantic != nil {
		return
	}
	for _, byteValue := range data {
		if byteValue == '\n' {
			f.finishLine()
			if f.semantic != nil {
				return
			}
			continue
		}
		if len(f.line) >= f.maxBytes {
			f.line = nil
			f.semantic = ErrJSONLLineTooLong
			return
		}
		f.line = append(f.line, byteValue)
	}
}

// Finish flushes a final unterminated nonempty line and freezes the framer.
func (f *JSONLFramer) Finish() {
	if f == nil || f.done {
		return
	}
	if f.semantic == nil && len(f.line) != 0 {
		f.finishLine()
	}
	f.line = nil
	f.done = true
}

func (f *JSONLFramer) finishLine() {
	line := f.line
	f.line = nil
	if f.callback == nil {
		return
	}
	if err := f.callback(line); err != nil {
		f.semantic = err
	}
}

// SemanticError returns the first semantic error reported by the callback or
// by the line bound. It never represents a reader/I/O failure.
func (f *JSONLFramer) SemanticError() error {
	if f == nil {
		return nil
	}
	return f.semantic
}

// ReadJSONL drains reader while framing bounded lines. The first return value
// is a provider-semantic error; the second is an operational reader fault.
// Both may be nonnil if a reader fails after a semantic fault.
func ReadJSONL(reader io.Reader, maxBytes int, callback func([]byte) error) (error, error) {
	if reader == nil {
		return nil, errors.New("nil JSONL reader")
	}
	framer := NewJSONLFramer(maxBytes, callback)
	if framer.semantic != nil {
		return framer.semantic, nil
	}
	buffer := make([]byte, jsonlReadBufferBytes)
	noProgress := 0
	for {
		n, err := reader.Read(buffer)
		if n < 0 || n > len(buffer) {
			return framer.SemanticError(), fmt.Errorf("reader returned invalid byte count %d", n)
		}
		if n > 0 {
			noProgress = 0
			framer.Feed(buffer[:n])
		} else if err == nil {
			noProgress++
			if noProgress >= 100 {
				return framer.SemanticError(), io.ErrNoProgress
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				framer.Finish()
				return framer.SemanticError(), nil
			}
			return framer.SemanticError(), err
		}
	}
}

// DrainReader consumes a stream to EOF while retaining no bytes. It is used
// for stderr and other evidence whose semantics are interpreted elsewhere.
func DrainReader(reader io.Reader) error {
	if reader == nil {
		return errors.New("nil evidence reader")
	}
	buffer := make([]byte, jsonlReadBufferBytes)
	noProgress := 0
	for {
		n, err := reader.Read(buffer)
		if n < 0 || n > len(buffer) {
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
