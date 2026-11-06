package control

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"testing/iotest"
)

func TestObservePrivateRestoreRejectsCorruptSelectedBytes(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		for _, corruption := range []string{"empty", "truncated", "duplicate keys", "trailing JSON"} {
			t.Run(map[bool]string{false: "json ", true: "gzip "}[compressed]+corruption, func(t *testing.T) {
				f := newPrivateRestoreFixture(t, compressed)
				data := []byte(`{"tasks":{},"groups":{}}`)
				switch corruption {
				case "empty":
					data = nil
				case "truncated":
					data = []byte(`{"tasks":{`)
				case "duplicate keys":
					data = []byte(`{"tasks":{},"tasks":{},"groups":{}}`)
				case "trailing JSON":
					data = append(data, []byte(` {}`)...)
				}
				if compressed {
					data = privateRestoreGzip(t, data)
				}
				writePrivateRestoreBytes(t, f.state, data)
				requirePrivateRestoreFault(t, f, ErrPrivateRestoreCorrupt)
			})
		}
	}
}

func TestObservePrivateRestoreGzipRequiresOneCompleteMember(t *testing.T) {
	for _, corruption := range []string{"zero bytes", "header", "CRC", "length", "truncated footer", "concatenated", "trailing bytes"} {
		t.Run(corruption, func(t *testing.T) {
			f := newPrivateRestoreFixture(t, true)
			data := bytes.Clone(f.bytes)
			switch corruption {
			case "zero bytes":
				data = nil
			case "header":
				data[0] ^= 0xff
			case "CRC":
				data[len(data)-8] ^= 1
			case "length":
				data[len(data)-4] ^= 1
			case "truncated footer":
				data = data[:len(data)-1]
			case "concatenated":
				data = append(data, privateRestoreGzip(t, []byte(" "))...)
			case "trailing bytes":
				data = append(data, ' ')
			}
			writePrivateRestoreBytes(t, f.state, data)
			requirePrivateRestoreFault(t, f, ErrPrivateRestoreCorrupt)
		})
	}
}

func TestPrivateRestoreStreamPreservesReadAndCancellationFaults(t *testing.T) {
	data, err := os.ReadFile("../../testdata/status-4.0.4-queued.json")
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("restore read failed")
	for _, compressed := range []bool{false, true} {
		payload := data
		if compressed {
			payload = privateRestoreGzip(t, data)
		}
		for _, offset := range []int{0, len(payload) / 2, len(payload)} {
			reader := io.MultiReader(bytes.NewReader(payload[:offset]), iotest.ErrReader(fault))
			got, readErr := parsePrivateRestoreStream(t.Context(), reader, compressed, FixtureVersion)
			if !errors.Is(readErr, fault) || errors.Is(readErr, ErrPrivateRestoreCorrupt) || len(got.Jobs) != 0 || len(got.Groups) != 0 {
				t.Fatalf("read fault lost: compressed=%t offset=%d err=%v", compressed, offset, readErr)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		reader := privateRestoreCancelReader{reader: bytes.NewReader(payload), cancel: cancel}
		got, readErr := parsePrivateRestoreStream(ctx, reader, compressed, FixtureVersion)
		cancel()
		if !errors.Is(readErr, context.Canceled) || errors.Is(readErr, ErrPrivateRestoreCorrupt) || len(got.Jobs) != 0 {
			t.Fatalf("cancellation during read: %v", readErr)
		}
	}
}

func TestPrivateRestoreKeepsConcurrentReadCancellationAndParserFaults(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		fault := errors.New("independent input read fault")
		reader := privateRestoreCombinedFaultReader{cancel: cancel, fault: fault}
		got, err := parsePrivateRestoreStream(ctx, reader, compressed, FixtureVersion)
		cancel()
		if !errors.Is(err, context.Canceled) || !errors.Is(err, fault) || errors.Is(err, ErrPrivateRestoreCorrupt) || len(got.Jobs) != 0 || len(got.Groups) != 0 {
			t.Fatalf("concurrent causes lost or mislabeled: compressed=%t err=%v", compressed, err)
		}
	}
}

type privateRestoreCombinedFaultReader struct {
	cancel context.CancelFunc
	fault  error
}

func (r privateRestoreCombinedFaultReader) Read(buffer []byte) (int, error) {
	n := copy(buffer, "!")
	r.cancel()
	return n, r.fault
}

func TestPrivateRestoreClassifiesSourceTruncationAsReadFault(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		got, err := parsePrivateRestoreStream(t.Context(), iotest.ErrReader(io.ErrUnexpectedEOF), compressed, FixtureVersion)
		if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ErrPrivateRestoreCorrupt) || len(got.Jobs) != 0 {
			t.Fatalf("input I/O failure became decoder corruption: compressed=%t err=%v", compressed, err)
		}
	}
}

type privateRestoreCancelReader struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r privateRestoreCancelReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.cancel()
	return n, err
}

func TestPrivateRestoreStreamRetainsUnknownNativeStates(t *testing.T) {
	for _, state := range []string{`{"Locked":{"previous_status":{"Queued":{"enqueued_at":"2026-09-13T20:00:00Z"}}}}`, `{"Stashed":{"enqueue_at":"2026-09-13T20:00:00Z"}}`} {
		f := newPrivateRestoreFixture(t, false)
		data, err := os.ReadFile("../../testdata/status-4.0.4-queued.json")
		if err != nil {
			t.Fatal(err)
		}
		// The real schema fixture's status is replaced as a typed JSON value.
		record := privateRestoreUnknownStateBytes(t, data, state)
		writePrivateRestoreBytes(t, f.state, record)
		got := observePrivateRestoreFixture(t, f)
		if got.Snapshot().Jobs[0].State != StateUnknown {
			t.Fatal("raw restore state was rewritten as a known completion")
		}
	}
}
