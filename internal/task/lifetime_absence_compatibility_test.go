package task

import (
	"io/fs"
	"os"
	"testing"
)

func TestRecordAbsentPreservesOSNotExistCompatibility(t *testing.T) {
	for _, err := range []error{fs.ErrNotExist, os.ErrNotExist} {
		if !RecordAbsent(err) {
			t.Errorf("RecordAbsent(%v) = false, want true", err)
		}
	}
}
