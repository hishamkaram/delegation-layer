package taskdir

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPreparedIdentityUsesActualRecordBytes(t *testing.T) {
	td := supervisorTask(t, testStore(t))
	path := filepath.Join(td.Dir, "meta.json")
	original := readTestFile(t, path)
	noncanonical := append([]byte(" \n"), original...)
	writeTestFile(t, path, noncanonical)
	_, meta, specHash, metaHash, err := td.PreparedIdentity()
	must(t, err)
	canonical, err := task.MarshalCanonical(meta)
	must(t, err)
	if bytes.Equal(canonical, noncanonical) || metaHash == task.ComputeSHA256(canonical) {
		t.Fatal("byte identity was reconstructed from decoded fields")
	}
	if metaHash != task.ComputeSHA256(noncanonical) || specHash != task.ComputeSHA256(readTestFile(t, filepath.Join(td.Dir, "task.json"))) {
		t.Fatal("prepared identity does not describe actual record bytes")
	}
	permit, err := td.PrepareSubmission(meta.SupervisorConfig)
	must(t, err)
	record, err := permit.Record()
	must(t, err)
	must(t, permit.Release())
	if record.MetaSHA256 != metaHash || record.SpecSHA256 != specHash {
		t.Fatal("inspection identity differs from durable admission binding")
	}
}

func TestLogDescriptorsDistinguishLiveBytesFromSealedIdentity(t *testing.T) {
	td := supervisorTask(t, testStore(t))
	descriptors, err := td.LogDescriptors()
	must(t, err)
	checkLogDescriptors(t, td, descriptors, false, false)
	permit := consumeStart(t, td)
	writer, err := td.OpenRawWriter("stdout")
	must(t, err)
	_, err = writer.Write([]byte("answer\n"))
	must(t, err)
	descriptors, err = td.LogDescriptors()
	must(t, err)
	checkLogDescriptors(t, td, descriptors, true, false)
	must(t, writer.Close())
	_, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	must(t, permit.Release())
	descriptors, err = td.LogDescriptors()
	must(t, err)
	checkLogDescriptors(t, td, descriptors, true, true)
	for _, descriptor := range descriptors {
		data := readTestFile(t, descriptor.Path)
		if descriptor.Size == nil || *descriptor.Size != int64(len(data)) || descriptor.SHA256 != task.ComputeSHA256(data) {
			t.Fatal("sealed descriptor does not match exact raw bytes")
		}
	}
	collectOK(t, td)
	// Historical winner validation already tolerates an absent seal receipt.
	must(t, os.Remove(filepath.Join(td.Dir, "provider.exit")))
	descriptors, err = td.LogDescriptors()
	must(t, err)
	checkLogDescriptors(t, td, descriptors, true, true)
	writeTestFile(t, filepath.Join(td.Dir, "raw", "stdout"), []byte("damaged"))
	if _, err = td.LogDescriptors(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("damaged raw stream described as sealed: %v", err)
	}
}

func checkLogDescriptors(t *testing.T, td *TaskDir, descriptors []LogDescriptor, available, sealed bool) {
	t.Helper()
	if len(descriptors) != 2 {
		t.Fatalf("unexpected stream count: %d", len(descriptors))
	}
	for i, name := range []string{"stderr", "stdout"} {
		descriptor := descriptors[i]
		if descriptor.Path != filepath.Join(td.Dir, "raw", name) || descriptor.Available != available || descriptor.Sealed != sealed {
			t.Fatalf("wrong stream descriptor: %+v", descriptor)
		}
		if !sealed && (descriptor.Size != nil || descriptor.SHA256 != "") {
			t.Fatalf("live bytes acquired final identity: %+v", descriptor)
		}
	}
}
