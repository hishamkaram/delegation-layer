package taskdir

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
)

func TestEvidenceRecordViewsExpireWithCallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout")
	if err := os.WriteFile(path, []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	evidence := &sealedEvidence{streams: map[predicate.Stream]sealedStream{predicate.Stdout: {file: file, size: 13}}}
	var retained io.ReaderAt
	err = evidence.Read(predicate.Stdout, func(reader io.Reader) error {
		var ok bool
		retained, ok = reader.(io.ReaderAt)
		if !ok {
			return errors.New("scoped evidence does not support record views")
		}
		if _, readErr := io.Copy(io.Discard, reader); readErr != nil {
			return readErr
		}
		var record [6]byte
		n, readErr := retained.ReadAt(record[:], 6)
		if readErr != nil {
			return readErr
		}
		if n != 6 || string(record[:]) != "second" {
			return errors.New("record offset was affected by sequential reads")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var late [1]byte
	if _, err = retained.ReadAt(late[:], 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("retained view still readable: %v", err)
	}
	if err = evidence.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceTruncationCannotBecomeSemanticRejection(t *testing.T) {
	for _, random := range []bool{false, true} {
		t.Run(fmt.Sprint(random), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stdout")
			if err := os.WriteFile(path, []byte("complete evidence"), 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			evidence := &sealedEvidence{streams: map[predicate.Stream]sealedStream{predicate.Stdout: {file: file, size: 17}}}
			if err = os.Truncate(path, 3); err != nil {
				t.Fatal(err)
			}
			err = evidence.Read(predicate.Stdout, func(reader io.Reader) error {
				return swallowTruncatedEvidence(t, reader, random)
			})
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("evidence callback lost fault: %v", err)
			}
			if err = evidence.Close(); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("evidence close lost fault: %v", err)
			}
		})
	}
}

func swallowTruncatedEvidence(t *testing.T, reader io.Reader, random bool) error {
	t.Helper()
	var data [17]byte
	var readErr error
	if random {
		at, ok := reader.(io.ReaderAt)
		if !ok {
			return errors.New("random-access reader missing")
		}
		_, readErr = at.ReadAt(data[:], 0)
	} else {
		_, readErr = io.ReadFull(reader, data[:])
	}
	if !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Errorf("read error=%v", readErr)
	}
	// Deliberately swallow the adapter-visible fault. Core must retain it.
	return nil
}
