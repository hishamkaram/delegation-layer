package antigravity

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

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
