package taskdir

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func briefTask(t *testing.T, brief []byte) *TaskDir {
	t.Helper()
	s := testStore(t)
	req, meta, _ := preparedInput(t, s)
	req.BriefLength = int64(len(brief))
	req.BriefSHA256 = task.ComputeSHA256(brief)
	td, err := s.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	return td
}

// preparedEntryErrors runs every prepared-set entry over the same current files.
func preparedEntryErrors(td *TaskDir) map[string]error {
	_, _, recordsErr := td.PreparedRecords()
	_, briefErr := td.ReadBrief()
	_, _, _, _, contextErr := td.loadAndValidatePreparedSetContext(context.Background())
	f, executionErr := td.OpenBriefForExecution()
	if executionErr == nil {
		executionErr = f.Close()
	}
	return map[string]error{"PreparedRecords": recordsErr, "ReadBrief": briefErr, "PreparedSetContext": contextErr, "OpenBriefForExecution": executionErr}
}

// requirePreparedEntries requires want (nil for success) from every entry and
// rejects a lower-precedence cause when reject is not nil.
func requirePreparedEntries(t *testing.T, td *TaskDir, want, reject error) {
	t.Helper()
	for name, err := range preparedEntryErrors(td) {
		if want == nil && err != nil || want != nil && !errors.Is(err, want) {
			t.Fatalf("%s error = %v, want %v", name, err, want)
		}
		if reject != nil && errors.Is(err, reject) {
			t.Fatalf("%s error = %v, must not report %v", name, err, reject)
		}
	}
}

func TestPreparedBriefIdentityUsesCurrentBytes(t *testing.T) {
	cases := []struct {
		name  string
		brief []byte
		want  error
	}{
		{"exact", []byte("hello brief"), nil},
		{"truncated", []byte("hello brie"), task.ErrIdentityMismatch},
		{"same length changed", []byte("hello BRIEF"), task.ErrIdentityMismatch},
		{"grown", []byte("hello brief!"), task.ErrIdentityMismatch},
		{"oversized", make([]byte, task.MaxBriefSize+1), task.ErrControlRecordTooBig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			td := preparedTask(t, testStore(t))
			writeTestFile(t, filepath.Join(td.Dir, "brief.md"), tc.brief)
			requirePreparedEntries(t, td, tc.want, nil)
		})
	}
}

func TestPreparedBriefAtMaximumSize(t *testing.T) {
	brief := bytes.Repeat([]byte("m"), task.MaxBriefSize)
	td := briefTask(t, brief)
	requirePreparedEntries(t, td, nil, nil)
	read, err := td.ReadBrief()
	must(t, err)
	if !bytes.Equal(read, brief) {
		t.Fatal("ReadBrief changed a maximum-size brief")
	}
	f, err := td.OpenBriefForExecution()
	must(t, err)
	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil || offset != 0 {
		closeFileQuietly(f)
		t.Fatalf("execution descriptor offset = %d, %v; want rewound to 0", offset, err)
	}
	executed, err := io.ReadAll(f)
	must(t, errors.Join(err, f.Close()))
	if !bytes.Equal(executed, brief) {
		t.Fatal("execution descriptor changed a maximum-size brief")
	}
	writeTestFile(t, filepath.Join(td.Dir, "brief.md"), append(brief, 'm'))
	requirePreparedEntries(t, td, task.ErrControlRecordTooBig, nil)
}

func TestReadBriefReturnsCallerOwnedExactCopy(t *testing.T) {
	td := preparedTask(t, testStore(t))
	first, err := td.ReadBrief()
	must(t, err)
	if !bytes.Equal(first, []byte("hello brief")) {
		t.Fatalf("ReadBrief = %q", first)
	}
	first[0] = 'X'
	second, err := td.ReadBrief()
	must(t, err)
	if !bytes.Equal(second, []byte("hello brief")) || !bytes.Equal(readTestFile(t, filepath.Join(td.Dir, "brief.md")), []byte("hello brief")) {
		t.Fatalf("caller mutation reached the prepared brief: %q", second)
	}
}

func TestPreparedBriefFaultPrecedence(t *testing.T) {
	duplicate := []byte(`{"a":1,"a":2}`)
	cases := []struct {
		name         string
		brief        []byte
		record       string
		want, reject error
	}{
		{"brief read fault before request JSON", make([]byte, task.MaxBriefSize+1), "task.json", task.ErrControlRecordTooBig, task.ErrDuplicateKey},
		{"request JSON before brief identity", []byte("hello BRIEF"), "task.json", task.ErrDuplicateKey, task.ErrIdentityMismatch},
		{"metadata JSON before brief identity", []byte("hello BRIEF"), "meta.json", task.ErrDuplicateKey, task.ErrIdentityMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			td := preparedTask(t, testStore(t))
			writeTestFile(t, filepath.Join(td.Dir, "brief.md"), tc.brief)
			writeTestFile(t, filepath.Join(td.Dir, tc.record), duplicate)
			requirePreparedEntries(t, td, tc.want, tc.reject)
		})
	}
}

func TestPreparedSetHashesRawControlBytes(t *testing.T) {
	td := supervisorTask(t, testStore(t))
	supervisorSubmit(t, td)
	_, _, spec, metaHash, err := td.loadAndValidatePreparedSet()
	must(t, err)
	if spec != task.ComputeSHA256(readTestFile(t, filepath.Join(td.Dir, "task.json"))) || metaHash != task.ComputeSHA256(readTestFile(t, filepath.Join(td.Dir, "meta.json"))) {
		t.Fatal("prepared hashes are not the raw record digests")
	}
	for _, name := range []string{"task.json", "meta.json"} {
		path := filepath.Join(td.Dir, name)
		original := readTestFile(t, path)
		// Trailing whitespace decodes identically but changes the raw digest.
		writeTestFile(t, path, append(append([]byte(nil), original...), '\n'))
		requirePreparedEntries(t, td, task.ErrIdentityMismatch, nil)
		writeTestFile(t, path, original)
		requirePreparedEntries(t, td, nil, nil)
	}
}

func TestPreparedGuardsValidateSubmitBeforeStart(t *testing.T) {
	badSchema := func(r *task.SubmitRecord) { r.SchemaVersion++ }
	badSpec := func(r *task.SubmitRecord) { r.SpecSHA256 = task.ComputeSHA256([]byte("other")) }
	cases := []struct {
		name         string
		brief        []byte
		submit       func(*task.SubmitRecord)
		start        func(*task.ProviderStartRecord)
		want, reject error
	}{
		{"submit identity before start schema", nil, badSpec, func(r *task.ProviderStartRecord) { r.SchemaVersion++ }, task.ErrIdentityMismatch, task.ErrUnsupportedSchema},
		{"submit schema before start identity", nil, badSchema, func(r *task.ProviderStartRecord) { r.BudgetNanos++ }, task.ErrUnsupportedSchema, task.ErrIdentityMismatch},
		{"brief before guards", make([]byte, task.MaxBriefSize+1), badSchema, nil, task.ErrControlRecordTooBig, task.ErrUnsupportedSchema},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			td := supervisorTask(t, testStore(t))
			submit := supervisorSubmit(t, td)
			supervisorReceipt(t, td)
			runner := consumeStart(t, td)
			start, err := runner.Record()
			must(t, err)
			must(t, runner.Release())
			mutateStartGuardForTest(t, td, start, tc.start)
			tc.submit(submit)
			data, err := task.MarshalCanonical(submit)
			must(t, err)
			writeTestFile(t, filepath.Join(td.Dir, "submit.json"), data)
			if tc.brief != nil {
				writeTestFile(t, filepath.Join(td.Dir, "brief.md"), tc.brief)
			}
			requirePreparedEntries(t, td, tc.want, tc.reject)
		})
	}
}

func TestPreparedSetContextChecksBeforeAndAfterReads(t *testing.T) {
	td := preparedTask(t, testStore(t))
	request, meta, spec, metaHash, err := td.loadAndValidatePreparedSetContext(context.Background())
	must(t, err)
	wantRequest, wantMeta, wantSpec, wantMetaHash, err := td.loadAndValidatePreparedSet()
	must(t, err)
	if request.TaskID != wantRequest.TaskID || meta.TaskID != wantMeta.TaskID || spec != wantSpec || metaHash != wantMetaHash {
		t.Fatal("context prepared set differs from the plain prepared set")
	}
	// An oversized brief proves cancellation is observed before any read.
	writeTestFile(t, filepath.Join(td.Dir, "brief.md"), make([]byte, task.MaxBriefSize+1))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var missing context.Context
	for name, ctx := range map[string]context.Context{"nil": missing, "canceled": canceled} {
		if _, _, _, _, err = td.loadAndValidatePreparedSetContext(ctx); !errors.Is(err, context.Canceled) || errors.Is(err, task.ErrControlRecordTooBig) {
			t.Fatalf("%s context prepared set error = %v, want only context.Canceled", name, err)
		}
		if _, _, err = td.store.readDigestContext(ctx, filepath.Join(td.Dir, "brief.md"), task.MaxBriefSize); !errors.Is(err, context.Canceled) || errors.Is(err, task.ErrControlRecordTooBig) {
			t.Fatalf("%s context digest error = %v, want only context.Canceled", name, err)
		}
	}
}

// TestReadDigestMatchesReadBytesBoundaries proves the digest reader keeps the
// rooted open, private-file, and bound outcomes of readBytes exactly.
func TestReadDigestMatchesReadBytesBoundaries(t *testing.T) {
	td := preparedTask(t, testStore(t))
	content := bytes.Repeat([]byte("r"), 64)
	regular := filepath.Join(td.Dir, "regular")
	writeTestFile(t, regular, content)
	loose := filepath.Join(td.Dir, "loose")
	writeTestFile(t, loose, content)
	must(t, os.Chmod(loose, 0o644))
	must(t, os.Symlink(regular, filepath.Join(td.Dir, "link")))
	must(t, makeFIFO(filepath.Join(td.Dir, "fifo")))
	must(t, os.Mkdir(filepath.Join(td.Dir, "directory"), 0o700))
	must(t, os.Symlink(td.Dir, filepath.Join(td.Dir, "parent")))
	// A nil want with fails set requires an identical failure whose errno is
	// platform-specific, such as a refused symlink leaf or parent.
	cases := []struct {
		name  string
		limit int64
		fails bool
		want  error
	}{
		{"regular", 64, false, nil},
		{"regular", 63, true, task.ErrControlRecordTooBig},
		{"missing", 64, true, os.ErrNotExist},
		{"loose", 64, true, task.ErrEvidenceFault},
		{"link", 64, true, nil},
		{"fifo", 64, true, task.ErrEvidenceFault},
		{"directory", 64, true, task.ErrEvidenceFault},
		{filepath.Join("parent", "regular"), 64, true, nil},
	}
	for _, tc := range cases {
		path := filepath.Join(td.Dir, tc.name)
		data, bytesErr := td.store.readBytes(path, tc.limit)
		digest, size, digestErr := td.store.readDigest(path, tc.limit)
		if !tc.fails {
			if bytesErr != nil || digestErr != nil || digest != task.ComputeSHA256(data) || size != int64(len(data)) {
				t.Fatalf("%s limit %d: bytes err=%v digest err=%v", tc.name, tc.limit, bytesErr, digestErr)
			}
			continue
		}
		if digestErr == nil || bytesErr == nil || tc.want != nil && !errors.Is(digestErr, tc.want) || digestErr.Error() != bytesErr.Error() {
			t.Fatalf("%s limit %d: digest err=%v, bytes err=%v, want %v", tc.name, tc.limit, digestErr, bytesErr, tc.want)
		}
	}
}

func TestDigestScratchSizeClampsUntrustedHint(t *testing.T) {
	for hint, want := range map[int64]int{
		-1:                     minDigestScratch,
		0:                      minDigestScratch,
		minDigestScratch - 1:   minDigestScratch,
		4096:                   4096,
		maxDigestScratch:       maxDigestScratch,
		maxDigestScratch + 1:   maxDigestScratch,
		int64(^uint64(0) >> 1): maxDigestScratch,
	} {
		if got := digestScratchSize(hint); got != want {
			t.Fatalf("digestScratchSize(%d) = %d, want %d", hint, got, want)
		}
	}
}
