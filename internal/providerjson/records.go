package providerjson

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// Framer records newline boundaries only. Bytes have already been captured by
// the core; it supplies scoped views through visit and retains no record data.
type Framer struct {
	offset, start int64
	line          uint64
	visit         func(int64, int64, uint64) error
	fault         error
	done          bool
}

func NewFramer(visit func(int64, int64, uint64) error) *Framer { return &Framer{visit: visit} }
func (f *Framer) Feed(data []byte) {
	if f.done {
		return
	}
	for _, b := range data {
		f.offset++
		if b == '\n' {
			f.deliver(f.offset - 1)
			f.start = f.offset
		}
	}
}

func (f *Framer) deliver(end int64) {
	f.line++
	if f.fault == nil && f.visit != nil {
		f.fault = f.visit(f.start, end, f.line)
	}
}

func (f *Framer) Finish() error {
	if !f.done {
		if f.offset > f.start {
			f.deliver(f.offset)
		}
		f.done = true
	}
	return f.fault
}

// Records visits every complete JSONL record; semantic faults do not stop reads.
// Operational read faults remain independent of callback faults.
func Records(r io.Reader, at io.ReaderAt, visit func(Value, uint64) error) (error, error) {
	if r == nil || at == nil {
		return nil, errors.New("nil native evidence reader")
	}
	f := NewFramer(func(start, end int64, line uint64) error {
		return visit(Value{Reader: at, Start: start, End: end}, line)
	})
	var buffer [BufferSize]byte
	noProgress := 0
	for {
		n, err := r.Read(buffer[:])
		if n < 0 || n > len(buffer) {
			return f.Finish(), errors.New("invalid native reader byte count")
		}
		if n > 0 {
			f.Feed(buffer[:n])
			noProgress = 0
		} else if err == nil {
			noProgress++
			if noProgress >= 100 {
				return f.Finish(), io.ErrNoProgress
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return f.Finish(), nil
			}
			return f.Finish(), err
		}
	}
}

// ReadRecords uses the core's scoped random-access evidence reader when one is
// available. Stream-only sources are spooled to a temporary file so a visitor
// can still select a late discriminator without retaining preceding payloads.
func ReadRecords(r io.Reader, visit func(Value, uint64) error) (semanticErr, readErr error) {
	if at, ok := r.(io.ReaderAt); ok {
		return Records(r, at, visit)
	}
	spool, err := os.CreateTemp("", "delegation-layer-providerjson-")
	if err != nil {
		return nil, err
	}
	name := spool.Name()
	defer func() {
		closeErr := spool.Close()
		removeErr := os.Remove(name)
		readErr = errors.Join(readErr, closeErr, removeErr)
	}()
	return Records(io.TeeReader(r, spool), spool, visit)
}

// Document drains one captured JSON document and returns a bounded view. Its
// reader remains owned by the surrounding evidence callback.
func Document(r io.Reader) (Value, error) {
	at, ok := r.(io.ReaderAt)
	if !ok {
		data, err := io.ReadAll(r)
		if err != nil {
			return Value{}, err
		}
		return Value{Reader: bytes.NewReader(data), End: int64(len(data))}, nil
	}
	var buffer [BufferSize]byte
	var size int64
	noProgress := 0
	for {
		n, err := r.Read(buffer[:])
		if n < 0 || n > len(buffer) {
			return Value{}, errors.New("invalid native reader byte count")
		}
		size += int64(n)
		if n > 0 {
			noProgress = 0
		} else if err == nil {
			noProgress++
			if noProgress >= 100 {
				return Value{}, io.ErrNoProgress
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return Value{Reader: at, End: size}, nil
			}
			return Value{}, err
		}
	}
}
