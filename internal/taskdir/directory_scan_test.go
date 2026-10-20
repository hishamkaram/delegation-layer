package taskdir

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// These are I/O error-contract tests for the shared directory scanner. Go
// 1.27.2's native Readdirnames(1) never returns names together with a
// non-EOF error, so the combined cases are boundary-contract cases, not a
// reproduced native fault.

type scriptedDirectoryRead struct {
	names []string
	err   error
}

type scriptedDirectory struct {
	reads    []scriptedDirectoryRead
	closeErr error
	calls    int
	closes   int
}

func (d *scriptedDirectory) Readdirnames(n int) ([]string, error) {
	if n != 1 {
		return nil, errors.New("scanner must stream one name at a time")
	}
	d.calls++
	if len(d.reads) == 0 {
		return []string{}, io.EOF
	}
	next := d.reads[0]
	d.reads = d.reads[1:]
	return next.names, next.err
}

func (d *scriptedDirectory) Close() error {
	d.closes++
	return d.closeErr
}

var (
	errTestReadFault  = errors.New("readdirent fault")
	errTestVisitFault = errors.New("visitor fault")
	errTestCloseFault = errors.New("close fault")
)

type directoryScanCase struct {
	name        string
	reads       []scriptedDirectoryRead
	closeErr    error
	visitFault  string
	wantVisited []string
	wantErr     []error
}

func TestScanDirectoryNamesComposesEveryFault(t *testing.T) {
	tests := []directoryScanCase{
		{name: "plain EOF", reads: []scriptedDirectoryRead{{names: []string{"a"}}, {names: []string{"b"}}}, wantVisited: []string{"a", "b"}},
		{name: "names with EOF", reads: []scriptedDirectoryRead{{names: []string{"a"}, err: io.EOF}}, wantVisited: []string{"a"}},
		{name: "joined EOF only", reads: []scriptedDirectoryRead{{names: []string{"a"}, err: errors.Join(io.EOF, io.EOF)}}, wantVisited: []string{"a"}},
		{name: "joined EOF and fault", reads: []scriptedDirectoryRead{{err: errors.Join(io.EOF, errTestReadFault)}}, wantErr: []error{errTestReadFault}},
		{name: "read fault after names", reads: []scriptedDirectoryRead{{names: []string{"a"}}, {err: errTestReadFault}}, wantVisited: []string{"a"}, wantErr: []error{errTestReadFault}},
		{
			name: "names with visit and read faults", reads: []scriptedDirectoryRead{{names: []string{"bad"}, err: errTestReadFault}},
			visitFault: "bad", wantVisited: []string{"bad"}, wantErr: []error{errTestVisitFault, errTestReadFault},
		},
		{name: "visit fault stops scan", reads: []scriptedDirectoryRead{{names: []string{"bad"}}, {names: []string{"later"}}}, visitFault: "bad", wantVisited: []string{"bad"}, wantErr: []error{errTestVisitFault}},
		{name: "close fault", reads: []scriptedDirectoryRead{{names: []string{"a"}}}, closeErr: errTestCloseFault, wantVisited: []string{"a"}, wantErr: []error{errTestCloseFault}},
		{
			name: "visit and close faults", reads: []scriptedDirectoryRead{{names: []string{"bad"}}}, closeErr: errTestCloseFault,
			visitFault: "bad", wantVisited: []string{"bad"}, wantErr: []error{errTestVisitFault, errTestCloseFault},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := &scriptedDirectory{reads: test.reads, closeErr: test.closeErr}
			var visited []string
			err := scanDirectoryNames(notCanceled, dir, func(name string) error {
				visited = append(visited, name)
				if name == test.visitFault {
					return errTestVisitFault
				}
				return nil
			})
			assertDirectoryScan(t, err, test.wantErr, dir)
			if !slices.Equal(visited, test.wantVisited) {
				t.Fatalf("visited = %v, want %v", visited, test.wantVisited)
			}
		})
	}
}

func assertDirectoryScan(t *testing.T, err error, wants []error, dir *scriptedDirectory) {
	t.Helper()
	if len(wants) == 0 && err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, want := range wants {
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
	}
	if dir.closes != 1 {
		t.Fatalf("Close calls = %d, want exactly one", dir.closes)
	}
}

func TestScanDirectoryNamesHonorsCancellation(t *testing.T) {
	t.Run("before first read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		dir := &scriptedDirectory{reads: []scriptedDirectoryRead{{names: []string{"a"}}}, closeErr: errTestCloseFault}
		err := scanDirectoryNames(func() error { return ctx.Err() }, dir, func(string) error { return nil })
		assertDirectoryScan(t, err, []error{context.Canceled, errTestCloseFault}, dir)
		if dir.calls != 0 {
			t.Fatalf("Readdirnames calls = %d after cancellation, want 0", dir.calls)
		}
	})
	t.Run("during scan", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		dir := &scriptedDirectory{reads: []scriptedDirectoryRead{{names: []string{"a"}}, {names: []string{"b"}}}}
		var visited []string
		err := scanDirectoryNames(func() error { return ctx.Err() }, dir, func(name string) error {
			visited = append(visited, name)
			cancel()
			return nil
		})
		assertDirectoryScan(t, err, []error{context.Canceled}, dir)
		if !slices.Equal(visited, []string{"a"}) {
			t.Fatalf("visited = %v after cancellation, want [a]", visited)
		}
	})
}

// The launch inventory keeps its parse identity and the independent read
// fault when both arrive in one directory read.
func TestLifetimeLaunchInventoryKeepsParseAndReadFaults(t *testing.T) {
	dir := &scriptedDirectory{reads: []scriptedDirectoryRead{{names: []string{"x" + lifetimeLaunchSuffix}, err: errTestReadFault}}}
	sequences, err := scanLifetimeLaunchInventory(context.Background(), dir, taskLifetimeLaunchNaming())
	assertDirectoryScan(t, err, []error{task.ErrIdentityMismatch, errTestReadFault}, dir)
	if sequences != nil {
		t.Fatalf("faulted inventory returned %v", sequences)
	}
}

func TestStopRecordDiscoveryKeepsAbsenceAndEntryIdentity(t *testing.T) {
	td := preparedTask(t, testStore(t))
	ids, err := td.discoverStopRecordIDs()
	if err != nil || ids.entries != 0 || len(ids.requests) != 0 {
		t.Fatalf("absent stop directory = %+v, %v; want empty and nil", ids, err)
	}
	stopDir := filepath.Join(td.Dir, "stop")
	must(t, os.Mkdir(stopDir, 0o700))
	ids, err = td.discoverStopRecordIDs()
	if err != nil || ids.entries != 0 {
		t.Fatalf("empty stop directory = %+v, %v; want empty and nil", ids, err)
	}
	must(t, os.WriteFile(filepath.Join(stopDir, "unexpected"), nil, 0o600))
	if _, err = td.discoverStopRecordIDs(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unexpected stop entry = %v, want evidence fault", err)
	}
}
