package resultread

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestReadVerifiesHiddenTailBeforeReleasingRange(t *testing.T) {
	data := []byte("zero: selected range; hidden authoritative tail")
	descriptor := task.PayloadDescriptor{Basename: "result.txt", Length: int64(len(data)), SHA256: task.ComputeSHA256(data)}
	got, err := Read(t.Context(), bytes.NewReader(data), descriptor, 6, 8)
	if err != nil || string(got.Data) != "selected" || got.NextOffset != 14 || got.EOF {
		t.Fatalf("range=%+v error=%v", got, err)
	}
	data[len(data)-1] ^= 1
	got, err = Read(t.Context(), bytes.NewReader(data), descriptor, 6, 8)
	if !errors.Is(err, task.ErrEvidenceFault) || len(got.Data) != 0 {
		t.Fatalf("hidden corruption released bytes: %+v %v", got, err)
	}
}

type finalCancelReader struct {
	source io.Reader
	cancel context.CancelFunc
}

func (r finalCancelReader) Read(data []byte) (int, error) {
	n, err := r.source.Read(data)
	if errors.Is(err, io.EOF) {
		r.cancel()
	}
	return n, err
}

func TestReadCancellationAtEOFReturnsNoTrustedBytes(t *testing.T) {
	data := []byte("complete")
	descriptor := task.PayloadDescriptor{Basename: "result.txt", Length: int64(len(data)), SHA256: task.ComputeSHA256(data)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	got, err := Read(ctx, finalCancelReader{bytes.NewReader(data), cancel}, descriptor, 0, 5)
	if !errors.Is(err, context.Canceled) || len(got.Data) != 0 {
		t.Fatalf("cancelled proof=%+v %v", got, err)
	}
}

func TestReadRepresentative64MiBPayloadWithBoundedRange(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefgh"), 8<<20)
	descriptor := task.PayloadDescriptor{Basename: "result.txt", Length: int64(len(data)), SHA256: task.ComputeSHA256(data)}
	got, err := Read(t.Context(), bytes.NewReader(data), descriptor, descriptor.Length-8, 65536)
	if err != nil || string(got.Data) != "abcdefgh" || !got.EOF {
		t.Fatalf("64MiB final range=%+v %v", got, err)
	}
	for _, changed := range [][]byte{data[:len(data)-1], append(data, byte('x'))} {
		got, err = Read(t.Context(), bytes.NewReader(changed), descriptor, 0, 1)
		if !errors.Is(err, task.ErrEvidenceFault) || len(got.Data) != 0 {
			t.Fatalf("invalid length released bytes: %v", err)
		}
	}
}

func TestCopyVerifiesCompletePayload(t *testing.T) {
	data := bytes.Repeat([]byte("answer€"), 15000)
	descriptor := task.PayloadDescriptor{Basename: "result.txt", Length: int64(len(data)), SHA256: task.ComputeSHA256(data)}
	for _, scenario := range []struct {
		name  string
		data  []byte
		fault bool
	}{
		{"complete", data, false}, {"short", data[:len(data)-1], true}, {"long", append(append([]byte{}, data...), 'x'), true}, {"corrupt", bytes.Repeat([]byte("x"), len(data)), true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var destination bytes.Buffer
			err := copyVerified(t.Context(), bytes.NewReader(scenario.data), descriptor, &destination)
			if scenario.fault {
				if !errors.Is(err, task.ErrEvidenceFault) {
					t.Fatalf("expected evidence fault: %v", err)
				}
			} else if err != nil || !bytes.Equal(destination.Bytes(), data) {
				t.Fatalf("copy mismatch: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var output bytes.Buffer
	if err := copyVerified(ctx, finalCancelReader{bytes.NewReader(data), cancel}, descriptor, &output); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled copy: %v", err)
	}
	if err := copyVerified(t.Context(), bytes.NewReader(data), descriptor, shortWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %v", err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestCopyRejectsEmptyPayload(t *testing.T) {
	descriptor := task.PayloadDescriptor{Basename: "result.txt", SHA256: task.ComputeSHA256(nil)}
	var output bytes.Buffer
	if err := copyVerified(t.Context(), bytes.NewReader(nil), descriptor, &output); !errors.Is(err, task.ErrInvariantFault) || output.Len() != 0 {
		t.Fatalf("empty invalid payload accepted: %v", err)
	}
}
