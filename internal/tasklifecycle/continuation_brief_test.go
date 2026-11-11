package tasklifecycle_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

var edgeBriefUnit = []byte("\xef\xbb\xbf\x00\r\nline\r\n\xff\xfe\x00 trailing space \n\n")

// edgeBrief returns size bytes of edge-case content that a text-oriented
// reader could alter: a BOM, NULs, CRLF, invalid UTF-8, and trailing space.
func edgeBrief(size int) []byte {
	return bytes.Repeat(edgeBriefUnit, size/len(edgeBriefUnit)+1)[:size]
}

// continuationBriefs returns valid briefs whose content must survive
// unchanged: nothing is trimmed, normalized, or decoded. The sizes straddle
// the streaming scratch bounds (512 B and 32 KiB) and the brief size limit.
// It is called by the test that uses it, so the size-limit brief is never
// allocated by package initialization or by a run that skips that test.
func continuationBriefs() []struct {
	name  string
	brief []byte
} {
	return []struct {
		name  string
		brief []byte
	}{
		{"single byte", []byte{'x'}},
		{"edge bytes", edgeBrief(len(edgeBriefUnit))},
		{"no trailing newline", []byte("no newline")},
		{"whitespace only", []byte(" \t\r\n ")},
		{"below scratch min", edgeBrief(511)},
		{"scratch min", edgeBrief(512)},
		{"above scratch min", edgeBrief(513)},
		{"below scratch max", edgeBrief(32*1024 - 1)},
		{"above scratch max", edgeBrief(32*1024 + 1)},
		{"size limit", edgeBrief(task.MaxBriefSize)},
	}
}

// createBriefTask stores a task with brief beside a fixture task, cloning the
// fixture's otherwise valid request and binding.
func createBriefTask(t *testing.T, store *taskdir.Store, fixture *taskdir.TaskDir, id string, brief []byte) *taskdir.TaskDir {
	t.Helper()
	request, meta, err := fixture.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	clonedRequest, clonedMeta := *request, *meta
	clonedRequest.TaskID, clonedMeta.TaskID = id, id
	clonedRequest.BriefSHA256, clonedRequest.BriefLength = task.ComputeSHA256(brief), int64(len(brief))
	td, err := store.CreateTask(id, &clonedRequest, brief, &clonedMeta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return td
}

// TestContinuationInputInheritsExactBriefBytes proves that a continuation
// reads back the validated predecessor brief byte for byte, and that the
// returned slice is the caller's: changing it neither changes the stored brief
// nor a later read.
func TestContinuationInputInheritsExactBriefBytes(t *testing.T) {
	store, fixture, _ := newBriefFixture(t)
	for index, test := range continuationBriefs() {
		t.Run(test.name, func(t *testing.T) {
			brief := test.brief
			id := string(bytes.Repeat([]byte{"0123456789abcdef"[index]}, 32))
			td := createBriefTask(t, store, fixture, id, brief)

			_, _, got, err := tasklifecycle.LoadContinuationInput(store.Root, tasklifecycle.DispatchRequest{TaskID: id}, continuationOpenOptions)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, brief) {
				t.Fatalf("continuation brief is %d bytes and differs from the %d-byte stored brief", len(got), len(brief))
			}
			clear(got)
			again, err := td.ReadBrief()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again, brief) {
				t.Fatal("mutating the returned continuation brief changed the stored brief")
			}
		})
	}
}

// TestContinuationInputRefusesChangedBrief proves that the continuation input
// keeps validating the stored brief against the recorded identity.
func TestContinuationInputRefusesChangedBrief(t *testing.T) {
	store, fixture, _ := newBriefFixture(t)
	brief := edgeBrief(len(edgeBriefUnit))
	td := createBriefTask(t, store, fixture, string(bytes.Repeat([]byte{'9'}, 32)), brief)
	briefPath := filepath.Join(td.Dir, "brief.md")
	for name, changed := range map[string][]byte{
		"truncated":   brief[:len(brief)-1],
		"same-length": append(bytes.Clone(brief[:len(brief)-1]), brief[len(brief)-1]^0xff),
	} {
		t.Run(name, func(t *testing.T) {
			writeBriefFile(t, briefPath, changed)
			_, _, got, err := tasklifecycle.LoadContinuationInput(store.Root, tasklifecycle.DispatchRequest{TaskID: td.TaskID}, continuationOpenOptions)
			if !errors.Is(err, task.ErrIdentityMismatch) || got != nil {
				t.Fatalf("changed brief = %d bytes, %v; want %v", len(got), err, task.ErrIdentityMismatch)
			}
			writeBriefFile(t, briefPath, brief)
			if _, _, got, err = tasklifecycle.LoadContinuationInput(store.Root, tasklifecycle.DispatchRequest{TaskID: td.TaskID}, continuationOpenOptions); err != nil || !bytes.Equal(got, brief) {
				t.Fatalf("restored brief = %d bytes, %v", len(got), err)
			}
		})
	}
}

func TestContinueMissingPredecessorOwnsOnlyInputStore(t *testing.T) {
	store, _, _ := newBriefFixture(t)
	assertContinueInputFailure(t, store.Root, string(bytes.Repeat([]byte{'9'}, 32)), os.ErrNotExist)
}

func TestContinueStoreOpenFailureKeepsPredecessorStage(t *testing.T) {
	store, td, _ := newBriefFixture(t)
	cause := errors.New("continuation store unavailable")
	options := tasklifecycle.ContinuationOptions{Dispatch: tasklifecycle.DispatchOptions{
		OpenStore: func(string, bool) (*taskdir.Store, error) { return nil, cause },
	}}
	result := tasklifecycle.ContinueContext(t.Context(), tasklifecycle.DispatchRequest{Root: store.Root, TaskID: td.TaskID}, options)
	var stage *admission.StageError
	if !errors.Is(result.Failure, cause) || !errors.As(result.Failure, &stage) || stage.Stage() != "load-predecessor" {
		t.Fatalf("store open failure = %v; want load-predecessor with original cause", result.Failure)
	}
	if result.Phase != tasklifecycle.ContinueLoadPredecessor || result.Dispatch != nil || result.Resources != nil || result.Workspace != nil {
		t.Fatalf("failed store open started successor work: %+v", result)
	}
}

func TestContinueRefusesChangedRootIdentity(t *testing.T) {
	store, td, _ := newBriefFixture(t)
	path := filepath.Join(store.Root, "root.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root task.RootRecord
	if err = task.DecodeStrict(content, &root); err != nil {
		t.Fatal(err)
	}
	// Keep the root record valid while making its identity disagree with the
	// existing task and binding. A fresh reader must validate current metadata.
	first := byte('0')
	if root.RootID[0] == first {
		first = '1'
	}
	root.RootID = string(first) + root.RootID[1:]
	if err = task.ValidateRootRecord(&root); err != nil {
		t.Fatal(err)
	}
	content, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	assertContinueInputFailure(t, store.Root, td.TaskID, task.ErrIdentityMismatch)
}

func assertContinueInputFailure(t *testing.T, root, id string, want error) {
	t.Helper()
	var opened []*taskdir.Store
	options := tasklifecycle.ContinuationOptions{Dispatch: tasklifecycle.DispatchOptions{
		OpenStore: func(path string, create bool) (*taskdir.Store, error) {
			if path != root || create {
				t.Fatalf("continuation input open = %q, create=%v; want existing %q", path, create, root)
			}
			store, err := taskdir.OpenStore(path)
			if err != nil {
				return nil, err
			}
			opened = append(opened, store)
			t.Cleanup(func() {
				if closeErr := store.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			})
			return store, nil
		},
	}}
	result := tasklifecycle.ContinueContext(t.Context(), tasklifecycle.DispatchRequest{Root: root, TaskID: id}, options)
	var stage *admission.StageError
	if !errors.Is(result.Failure, want) || !errors.As(result.Failure, &stage) || stage.Stage() != "load-predecessor" {
		t.Errorf("continuation input failure = %v; want load-predecessor with %v", result.Failure, want)
	}
	if result.Phase != tasklifecycle.ContinueLoadPredecessor || result.Dispatch != nil || result.Resources != nil || result.Workspace != nil {
		t.Errorf("failed predecessor started successor work: %+v", result)
	}
	if len(opened) != 1 {
		t.Errorf("failed continuation opened %d Stores; only its input reader needs one", len(opened))
	}
	for _, store := range opened {
		if _, err := store.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
			t.Errorf("failed continuation left input Store open: %v", err)
		}
	}
}

// continuationOpenOptions opens existing stores only: LoadContinuationInput
// never creates one.
var continuationOpenOptions = tasklifecycle.DispatchOptions{OpenStore: func(root string, create bool) (*taskdir.Store, error) {
	if create {
		return nil, errors.New("continuation input must not create a store")
	}
	return taskdir.OpenStore(root)
}}

func newBriefFixture(t *testing.T) (*taskdir.Store, *taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	store, td, req := testsupport.NewAppTestTask(t, false)
	t.Cleanup(func() { testsupport.CloseAppTestTask(t, store, td) })
	return store, td, req
}

// Caller brief errors are correctable only after the complete predecessor read
// and its owned handles have closed successfully. No admission work may start.
func TestContinueInvalidCallerBriefIsRequestValidation(t *testing.T) {
	for _, name := range []string{"missing", "oversized", "conflicting"} {
		t.Run(name, func(t *testing.T) {
			store, td, _ := newBriefFixture(t)
			request := tasklifecycle.DispatchRequest{Root: store.Root, TaskID: td.TaskID}
			want := error(config.ErrBriefNotFound)
			switch name {
			case "missing":
				request.BriefPath = filepath.Join(t.TempDir(), "missing.md")
			case "oversized":
				request.BriefBytes = bytes.Repeat([]byte{'x'}, task.MaxBriefSize+1)
				want = config.ErrBriefTooLarge
			case "conflicting":
				request.BriefPath, request.BriefBytes = "unused.md", []byte("replacement")
				want = nil
			}
			result := tasklifecycle.ContinueContext(t.Context(), request, tasklifecycle.ContinuationOptions{Dispatch: continuationOpenOptions})
			var stage *admission.StageError
			if result.Failure == nil || !errors.As(result.Failure, &stage) || stage.Stage() != "validate-request" || result.Phase != tasklifecycle.ContinueValidation {
				t.Fatalf("caller brief failure attributed to predecessor: phase=%v failure=%v", result.Phase, result.Failure)
			}
			if want != nil && !errors.Is(result.Failure, want) {
				t.Fatalf("caller input cause was lost: %v", result.Failure)
			}
			if result.Dispatch != nil || result.Resources != nil || result.Workspace != nil {
				t.Fatal("invalid caller brief started successor work")
			}
		})
	}
}

func TestContinuationInputOptionalBriefPreservesExactBytes(t *testing.T) {
	store, td, _ := newBriefFixture(t)
	inherited, err := td.ReadBrief()
	if err != nil {
		t.Fatal(err)
	}
	override := edgeBrief(513)
	path := filepath.Join(t.TempDir(), "replacement.md")
	writeBriefFile(t, path, override)
	for _, test := range []struct {
		name    string
		path    string
		content []byte
		want    []byte
	}{
		{"omitted", "", nil, inherited},
		{"file", path, nil, override},
		{"bytes", "", override, override},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, got, readErr := tasklifecycle.LoadContinuationInputContext(t.Context(), store.Root, tasklifecycle.DispatchRequest{TaskID: td.TaskID, BriefPath: test.path, BriefBytes: test.content}, continuationOpenOptions)
			if readErr != nil || !bytes.Equal(got, test.want) {
				t.Fatalf("optional brief changed bytes: %v", readErr)
			}
			clear(got)
			again, readErr := td.ReadBrief()
			if readErr != nil || !bytes.Equal(again, inherited) || !bytes.Equal(override, edgeBrief(513)) {
				t.Fatal("returned brief aliases caller input or stored predecessor")
			}
		})
	}
}

func TestContinuePredecessorFaultsPrecedeCallerOverride(t *testing.T) {
	for _, name := range []string{"missing", "changed-brief", "ownership"} {
		t.Run(name, func(t *testing.T) {
			store, td, _ := newBriefFixture(t)
			request := tasklifecycle.DispatchRequest{Root: store.Root, TaskID: td.TaskID, BriefPath: filepath.Join(t.TempDir(), "missing-override.md")}
			var want error
			switch name {
			case "missing":
				request.TaskID, want = string(bytes.Repeat([]byte{'9'}, 32)), os.ErrNotExist
			case "changed-brief":
				writeBriefFile(t, filepath.Join(td.Dir, "brief.md"), []byte("changed"))
				want = task.ErrIdentityMismatch
			case "ownership":
				// The predecessor owner must refuse a directory in place of its
				// optional private ownership-record file before reading overrides.
				if err := os.Mkdir(filepath.Join(td.Dir, "runner-migration.json"), 0o700); err != nil {
					t.Fatal(err)
				}
				want = task.ErrEvidenceFault
			}
			result := tasklifecycle.ContinueContext(t.Context(), request, tasklifecycle.ContinuationOptions{Dispatch: continuationOpenOptions})
			var stage *admission.StageError
			if (want != nil && !errors.Is(result.Failure, want)) || !errors.As(result.Failure, &stage) || stage.Stage() != "load-predecessor" || result.Phase != tasklifecycle.ContinueLoadPredecessor {
				t.Fatalf("predecessor fault relabeled as caller input: %v", result.Failure)
			}
			if result.Dispatch != nil || result.Resources != nil || result.Workspace != nil {
				t.Fatal("unreadable predecessor started successor work")
			}
		})
	}
}

func TestContinuePredecessorStoreCloseFailurePrecedesOverrideRead(t *testing.T) {
	store, td, _ := newBriefFixture(t)
	options := tasklifecycle.DispatchOptions{OpenStore: func(root string, create bool) (*taskdir.Store, error) {
		opened, err := taskdir.OpenStore(root)
		if err != nil {
			return nil, err
		}
		// A real public lease retains the Store's maintenance handle. The input
		// loader must report its Close refusal before attempting caller I/O.
		lease, err := opened.AcquireRemoteByteUse(t.Context(), td.TaskID, true, false)
		if err != nil {
			return nil, errors.Join(err, opened.Close())
		}
		t.Cleanup(func() {
			if closeErr := errors.Join(lease.Close(), opened.Close()); closeErr != nil {
				t.Error(closeErr)
			}
		})
		return opened, nil
	}}
	result := tasklifecycle.ContinueContext(t.Context(), tasklifecycle.DispatchRequest{Root: store.Root, TaskID: td.TaskID, BriefPath: filepath.Join(t.TempDir(), "missing-override.md")}, tasklifecycle.ContinuationOptions{Dispatch: options})
	var stage *admission.StageError
	if !errors.Is(result.Failure, task.ErrLockBusy) || errors.Is(result.Failure, config.ErrBriefNotFound) || !errors.As(result.Failure, &stage) || stage.Stage() != "load-predecessor" || result.Phase != tasklifecycle.ContinueLoadPredecessor {
		t.Fatalf("predecessor Close failure did not retain precedence: %v", result.Failure)
	}
	if result.Dispatch != nil || result.Resources != nil || result.Workspace != nil {
		t.Fatal("predecessor Close refusal started successor work")
	}
}

func TestContinuePredecessorPathErrorIsNotCallerInput(t *testing.T) {
	store, td, _ := newBriefFixture(t)
	missingStore := filepath.Join(store.Root, "missing-store")
	options := tasklifecycle.DispatchOptions{OpenStore: func(string, bool) (*taskdir.Store, error) {
		return taskdir.OpenStore(missingStore)
	}}
	result := tasklifecycle.ContinueContext(t.Context(), tasklifecycle.DispatchRequest{Root: store.Root, TaskID: td.TaskID, BriefPath: filepath.Join(t.TempDir(), "missing-override.md")}, tasklifecycle.ContinuationOptions{Dispatch: options})
	var stage *admission.StageError
	var nativeRead *os.PathError
	if !errors.As(result.Failure, &nativeRead) || !errors.As(result.Failure, &stage) || stage.Stage() != "load-predecessor" || result.Phase != tasklifecycle.ContinueLoadPredecessor {
		t.Fatalf("predecessor-native PathError was relabeled as caller input: %v", result.Failure)
	}
	if errors.Is(result.Failure, config.ErrBriefNotFound) || result.Dispatch != nil || result.Resources != nil || result.Workspace != nil {
		t.Fatal("predecessor failure read caller input or started successor work")
	}
}
