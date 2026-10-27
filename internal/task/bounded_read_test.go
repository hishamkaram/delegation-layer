package task

import (
	"bytes"
	"errors"
	"testing"
)

const boundedTestLimit = 64

// boundedCountingReader records how many bytes its consumer pulled.
type boundedCountingReader struct {
	r    *bytes.Reader
	read int64
}

func (c *boundedCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

// overflowFaultReader returns every byte in one Read together with a fault.
type overflowFaultReader struct {
	data []byte
	err  error
}

func (o *overflowFaultReader) Read(p []byte) (int, error) {
	n := copy(p, o.data)
	o.data = o.data[n:]
	return n, o.err
}

func TestDigestBoundedMatchesReadBoundedAtLimit(t *testing.T) {
	for _, size := range []int{0, boundedTestLimit - 1, boundedTestLimit} {
		data := bytes.Repeat([]byte("d"), size)
		// A one-byte scratch buffer proves the digest streams through it.
		for _, scratch := range []int{1, 7, 4096} {
			digest, n, err := DigestBounded(bytes.NewReader(data), boundedTestLimit, make([]byte, scratch))
			if err != nil || digest != ComputeSHA256(data) || n != int64(size) {
				t.Fatalf("size %d scratch %d: digest=%s n=%d err=%v", size, scratch, digest, n, err)
			}
		}
		read, err := ReadBounded(bytes.NewReader(data), boundedTestLimit)
		if err != nil || !bytes.Equal(read, data) {
			t.Fatalf("ReadBounded size %d: %d bytes, %v", size, len(read), err)
		}
	}
}

func TestDigestBoundedRejectsOverflowAfterOneExtraByte(t *testing.T) {
	for _, size := range []int{boundedTestLimit + 1, 1 << 20} {
		source := &boundedCountingReader{r: bytes.NewReader(make([]byte, size))}
		digest, n, err := DigestBounded(source, boundedTestLimit, make([]byte, 16))
		if !errors.Is(err, ErrControlRecordTooBig) || errors.Unwrap(err) != nil || digest != "" || n != 0 {
			t.Fatalf("size %d: digest=%q n=%d err=%v, want bare ErrControlRecordTooBig", size, digest, n, err)
		}
		if source.read != boundedTestLimit+1 {
			t.Fatalf("size %d: consumed %d bytes, want %d", size, source.read, boundedTestLimit+1)
		}
		if _, err = ReadBounded(bytes.NewReader(make([]byte, size)), boundedTestLimit); !errors.Is(err, ErrControlRecordTooBig) || errors.Unwrap(err) != nil {
			t.Fatalf("ReadBounded size %d: %v, want bare ErrControlRecordTooBig", size, err)
		}
	}
}

func TestDigestBoundedReadFaultPrecedesOverflow(t *testing.T) {
	fault := errors.New("stream fault")
	oversized := make([]byte, boundedTestLimit+1)
	_, _, digestErr := DigestBounded(&overflowFaultReader{data: oversized, err: fault}, boundedTestLimit, make([]byte, 2*boundedTestLimit))
	_, readErr := ReadBounded(&overflowFaultReader{data: oversized, err: fault}, boundedTestLimit)
	for name, err := range map[string]error{"DigestBounded": digestErr, "ReadBounded": readErr} {
		if !errors.Is(err, fault) || errors.Is(err, ErrControlRecordTooBig) {
			t.Fatalf("%s error = %v, want only the read fault", name, err)
		}
	}
	if digestErr.Error() != readErr.Error() {
		t.Fatalf("read fault wrapping differs: %q vs %q", digestErr, readErr)
	}
}

func TestDigestBoundedRequiresScratchBuffer(t *testing.T) {
	for name, buffer := range map[string][]byte{"nil": nil, "empty": {}} {
		source := &boundedCountingReader{r: bytes.NewReader([]byte("data"))}
		digest, n, err := DigestBounded(source, boundedTestLimit, buffer)
		if !errors.Is(err, ErrInvariantFault) || digest != "" || n != 0 {
			t.Fatalf("%s buffer: digest=%q n=%d err=%v, want ErrInvariantFault", name, digest, n, err)
		}
		if source.read != 0 {
			t.Fatalf("%s buffer consumed %d bytes", name, source.read)
		}
	}
}
