package provider

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"
)

func TestJSONLFramerChunksAndFlushesFinalLine(t *testing.T) {
	var lines [][]byte
	framer := NewJSONLFramer(64, func(line []byte) error {
		lines = append(lines, slices.Clone(line))
		return nil
	})
	framer.Feed([]byte("one\ntw"))
	framer.Feed([]byte("o"))
	framer.Finish()
	framer.Feed([]byte("late\n"))
	if framer.SemanticError() != nil || !slices.EqualFunc(lines, [][]byte{[]byte("one"), []byte("two")}, bytes.Equal) {
		t.Fatalf("lines=%q semantic=%v", lines, framer.SemanticError())
	}
}

func TestJSONLFramerSemanticFaultBoundsAndStopsCallbacks(t *testing.T) {
	callbackErr := errors.New("semantic callback fault")
	called := 0
	framer := NewJSONLFramer(4, func([]byte) error {
		called++
		return callbackErr
	})
	framer.Feed([]byte("good\nlate\n"))
	if !errors.Is(framer.SemanticError(), callbackErr) || called != 1 {
		t.Fatalf("semantic=%v callbacks=%d", framer.SemanticError(), called)
	}

	framer = NewJSONLFramer(4, func([]byte) error {
		called++
		return nil
	})
	framer.Feed([]byte("12345\nvalid\n"))
	if !errors.Is(framer.SemanticError(), ErrJSONLLineTooLong) {
		t.Fatalf("oversize semantic=%v", framer.SemanticError())
	}
}

func TestReadJSONLSeparatesSemanticAndReaderFaults(t *testing.T) {
	semanticErr := errors.New("invalid event")
	semantic, readErr := ReadJSONL(bytes.NewReader([]byte("a\nb")), 64, func(line []byte) error {
		if string(line) == "a" {
			return semanticErr
		}
		return nil
	})
	if !errors.Is(semantic, semanticErr) || readErr != nil {
		t.Fatalf("semantic=%v read=%v", semantic, readErr)
	}

	readerErr := errors.New("capture read failed")
	semantic, readErr = ReadJSONL(&jsonlErrorReader{data: []byte("a\nb"), err: readerErr}, 64, func([]byte) error { return nil })
	if semantic != nil || !errors.Is(readErr, readerErr) {
		t.Fatalf("semantic=%v read=%v", semantic, readErr)
	}

	semantic, readErr = ReadJSONL(&jsonlErrorReader{data: []byte("a\n"), err: readerErr}, 64, func([]byte) error { return semanticErr })
	if !errors.Is(semantic, semanticErr) || !errors.Is(readErr, readerErr) {
		t.Fatalf("combined semantic=%v read=%v", semantic, readErr)
	}
}

func TestReadJSONLRejectsInvalidCountsAndNoProgress(t *testing.T) {
	semantic, readErr := ReadJSONL(invalidCountReader{}, 64, func([]byte) error { return nil })
	if semantic != nil || readErr == nil {
		t.Fatalf("invalid reader count semantic=%v read=%v", semantic, readErr)
	}
	semantic, readErr = ReadJSONL(stallReader{}, 64, func([]byte) error { return nil })
	if semantic != nil || !errors.Is(readErr, io.ErrNoProgress) {
		t.Fatalf("no-progress semantic=%v read=%v", semantic, readErr)
	}
}

func TestDrainReaderConsumesToEOFAndPropagatesFault(t *testing.T) {
	reader := &jsonlTrackingReader{data: []byte("diagnostic"), chunk: 2}
	if err := DrainReader(reader); err != nil || !reader.eof || reader.off != len(reader.data) {
		t.Fatalf("drain reader=%+v err=%v", reader, err)
	}
	readerErr := errors.New("stderr read failed")
	if err := DrainReader(&jsonlErrorReader{data: []byte("stderr"), err: readerErr}); !errors.Is(err, readerErr) {
		t.Fatalf("drain error=%v", err)
	}
}

type jsonlErrorReader struct {
	data []byte
	err  error
	off  int
}

func (r *jsonlErrorReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

type invalidCountReader struct{}

func (invalidCountReader) Read([]byte) (int, error) { return -1, nil }

type stallReader struct{}

func (stallReader) Read([]byte) (int, error) { return 0, nil }

type jsonlTrackingReader struct {
	data  []byte
	chunk int
	off   int
	eof   bool
}

func (r *jsonlTrackingReader) Read(p []byte) (int, error) {
	if r.off == len(r.data) {
		r.eof = true
		return 0, io.EOF
	}
	n := r.chunk
	if n > len(p) {
		n = len(p)
	}
	if remaining := len(r.data) - r.off; n > remaining {
		n = remaining
	}
	copy(p[:n], r.data[r.off:r.off+n])
	r.off += n
	return n, nil
}
