package providerjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func valueOf(s string) Value { return Value{Reader: strings.NewReader(s), End: int64(len(s))} }
func TestSelectedFieldsIgnoreOpaqueDuplicatesAndOrder(t *testing.T) {
	for _, s := range []string{`{"data":{"id":1,"id":2},"type":"completed"}`, `{"type":"completed","data":{"id":1,"id":2}}`} {
		f, err := valueOf(s).Object("type")
		if err != nil {
			t.Fatal(err)
		}
		got, err := f["type"].String(32)
		if err != nil || got != "completed" {
			t.Fatalf("type=%q error=%v", got, err)
		}
	}
	for _, s := range []string{`{"type":"success","type":"error"}`, `{"type":"success","TYPE":"error"}`, `{"t\u0079pe":"success","type":"error"}`} {
		_, err := valueOf(s).Object("type")
		var fault *FieldError
		if !errors.As(err, &fault) || fault.Code != "duplicate_control_field" {
			t.Fatalf("duplicate accepted: %v", err)
		}
	}
}

func TestValidateSyntaxEvenInsideDiscardedData(t *testing.T) {
	for _, s := range []string{`{"x":[1,]}`, `{"x":{"a":1,}}`, `{"x":"\ud800"}`, `{"x":"\udc00"}`, `{"x":01}`, `{"x":1e}`, `{"x":tru}`, `{"x":null} false`, "{\"x\":\"\xff\"}"} {
		if _, err := valueOf(s).Object("type"); err == nil {
			t.Fatalf("invalid JSON accepted: %q", s)
		}
	}
}

func TestStreamStringPreservesDecodedAnswer(t *testing.T) {
	var got bytes.Buffer
	err := valueOf(`"a\n\u0041\ud83d\ude00☃\\\/\t\b\f\r\""`).CopyString(&got)
	if err != nil || got.String() != "a\nA😀☃\\/\t\b\f\r\"" {
		t.Fatalf("got=%q err=%v", got.String(), err)
	}
	if _, err = valueOf(`"long answer"`).String(3); err == nil {
		t.Fatal("control bound ignored")
	}
}

func TestLargeOpaqueAndAnswerStreaming(t *testing.T) {
	for _, size := range []int{1 << 20, 32 << 20} {
		s := `{"opaque":"` + strings.Repeat("x", size) + `","answer":"` + strings.Repeat("y", size) + `"}`
		fields, err := valueOf(s).Object("answer")
		if err != nil {
			t.Fatal(err)
		}
		count := &countWriter{}
		if err = fields["answer"].CopyString(count); err != nil {
			t.Fatal(err)
		}
		if count.n != size || count.max > BufferSize {
			t.Fatalf("count=%+v", count)
		}
	}
}

type countWriter struct{ n, max int }

func (w *countWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	w.max = max(w.max, len(p))
	return len(p), nil
}

func TestArrayAndRecordBoundaries(t *testing.T) {
	var got []string
	err := valueOf(`["first",{"opaque":[1,2]},"last"]`).Array(func(v Value) error { b, e := v.Raw(128); got = append(got, string(b)); return e })
	if err != nil || len(got) != 3 {
		t.Fatalf("values=%v err=%v", got, err)
	}
	s := "{\"type\":\"first\"}\n{\"type\":\"last\"}"
	at := strings.NewReader(s)
	var lines []uint64
	semantic, readErr := Records(at, at, func(v Value, line uint64) error { lines = append(lines, line); return v.Validate() })
	if semantic != nil || readErr != nil || len(lines) != 2 {
		t.Fatalf("lines=%v semantic=%v read=%v", lines, semantic, readErr)
	}
}

type streamingOnlyReader struct{ io.Reader }

func TestStreamingOnlyRecordsAndDocumentsHaveRandomAccessFallback(t *testing.T) {
	stream := `{"type":"first"}
{"type":"last"}`
	var lines []string
	semantic, readErr := ReadRecords(streamingOnlyReader{Reader: strings.NewReader(stream)}, func(value Value, _ uint64) error {
		fields, err := value.Object("type")
		if err != nil {
			return err
		}
		decoded, err := fields["type"].String(32)
		if err == nil {
			lines = append(lines, decoded)
		}
		return err
	})
	if semantic != nil || readErr != nil || !slices.Equal(lines, []string{"first", "last"}) {
		t.Fatalf("lines=%v semantic=%v read=%v", lines, semantic, readErr)
	}
	document, err := Document(streamingOnlyReader{Reader: strings.NewReader(`{"answer":"streamed"}`)})
	if err != nil {
		t.Fatal(err)
	}
	fields, err := document.Object("answer")
	if err != nil {
		t.Fatal(err)
	}
	answer, err := fields["answer"].String(32)
	if err != nil || answer != "streamed" {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
}

func TestValidateUniqueKeysRejectsNestedCaseFoldedDuplicates(t *testing.T) {
	for _, input := range []string{
		`{"type":"assistant","metadata":{"same":1,"same":2}}`,
		`{"type":"assistant","metadata":{"same":1,"SAME":2}}`,
		`{"type":"assistant","content":[{"input":{"same":1,"SAME":2}}]}`,
	} {
		if err := valueOf(input).ValidateUniqueKeys(); err == nil {
			t.Fatalf("duplicate key accepted: %s", input)
		} else {
			var fieldErr *FieldError
			if !errors.As(err, &fieldErr) || fieldErr.Code != "duplicate_control_field" {
				t.Fatalf("duplicate error=%v", err)
			}
		}
	}
}

func TestOperationalAndWriterErrorsSurvive(t *testing.T) {
	want := errors.New("write failed")
	if err := valueOf(`"answer"`).CopyString(writerFunc(func([]byte) (int, error) { return 0, want })); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if err := valueOf(`"answer"`).CopyString(writerFunc(func([]byte) (int, error) { return 0, nil })); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}

func FuzzSelectedJSON(f *testing.F) {
	for _, s := range []string{`{"type":"ok"}`, `{"ignored":[1,true,null,"text"]}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 65536 {
			return
		}
		_, err := valueOf(s).Object("type")
		if err == nil {
			if !json.Valid([]byte(s)) {
				t.Fatalf("accepted invalid JSON: %q", s)
			}
			if validation := valueOf(s).Validate(); validation != nil {
				t.Fatal(validation)
			}
		}
	})
}

// syntheticString exposes a large encoded string without allocating its payload.
// The test measures parser allocation, not fixture construction.
type syntheticString struct{ size int64 }

func (r syntheticString) ReadAt(p []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative test offset")
	}
	if offset >= r.size+2 {
		return 0, io.EOF
	}
	n := min(len(p), int(r.size+2-offset))
	for i := range n {
		position := offset + int64(i)
		p[i] = 'x'
		if position == 0 || position == r.size+1 {
			p[i] = '"'
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestLargeStringAllocationIsBounded(t *testing.T) {
	const size = 128 << 20
	value := Value{Reader: syntheticString{size: size}, End: size + 2}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	writer := &countWriter{}
	if err := value.CopyString(writer); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if writer.n != size || writer.max > BufferSize {
		t.Fatalf("stream=%+v", writer)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("128 MiB answer allocated %d bytes; expected bounded parser memory", allocated)
	}
}

func TestMissingValueAndReaderLifetime(t *testing.T) {
	if _, err := (Value{}).String(10); err == nil {
		t.Fatal("missing control value accepted")
	}
	if err := valueOf(`"answer"`).CopyString(nil); err == nil {
		t.Fatal("nil writer accepted")
	}
	reader := &closedReaderAt{}
	value := Value{Reader: reader, End: 8}
	if err := value.CopyString(io.Discard); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("reader error=%v", err)
	}
}

type closedReaderAt struct{}

func (*closedReaderAt) ReadAt([]byte, int64) (int, error) { return 0, os.ErrClosed }
