package taskdir

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// countingReader records how many bytes its consumer actually pulled. It has
// no WriteTo, so a copy observes every Read.
type countingReader struct {
	r    io.Reader
	read int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

// failingReader returns its bytes, then a non-EOF fault.
type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func executionBriefRecord(brief []byte) *task.TaskRecord {
	return &task.TaskRecord{BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief))}
}

func TestVerifyExecutionBriefStopsAfterOneByteBeyondValidatedLength(t *testing.T) {
	brief := []byte("hello brief")
	req := executionBriefRecord(brief)
	grown := &countingReader{r: io.MultiReader(bytes.NewReader(brief), bytes.NewReader(make([]byte, 4<<20)))}
	if err := verifyExecutionBrief(grown, req); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("grown brief error = %v, want ErrIdentityMismatch", err)
	}
	if grown.read > req.BriefLength+1 {
		t.Fatalf("verifier consumed %d bytes of a grown brief, want <= %d", grown.read, req.BriefLength+1)
	}
}

func TestVerifyExecutionBriefRequiresExactContent(t *testing.T) {
	brief := []byte("hello brief")
	req := executionBriefRecord(brief)
	exact := &countingReader{r: bytes.NewReader(brief)}
	if err := verifyExecutionBrief(exact, req); err != nil {
		t.Fatalf("exact brief: %v", err)
	}
	if exact.read != req.BriefLength {
		t.Fatalf("exact brief consumed %d bytes, want %d", exact.read, req.BriefLength)
	}
	for name, data := range map[string][]byte{
		"truncated":       brief[:len(brief)-1],
		"same length":     []byte("hello BRIEF"),
		"one byte longer": append(append([]byte(nil), brief...), '!'),
		"empty":           nil,
	} {
		if err := verifyExecutionBrief(bytes.NewReader(data), req); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("%s brief error = %v, want ErrIdentityMismatch", name, err)
		}
	}
}

func TestVerifyExecutionBriefPreservesReadFault(t *testing.T) {
	brief := []byte("hello brief")
	fault := errors.New("descriptor read fault")
	err := verifyExecutionBrief(&failingReader{data: brief[:4], err: fault}, executionBriefRecord(brief))
	if !errors.Is(err, fault) || errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("read fault error = %v, want only the read fault", err)
	}
}
