package taskdir

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func remoteUseOwner(t *testing.T) (*Store, *task.TaskRecord) {
	t.Helper()
	owner := testStore(t)
	req, meta, brief := preparedInput(t, owner)
	req.Remote = &task.RemoteBinding{StorageLayout: 2, OriginRootID: strings.Repeat("a", 32), IntentSHA256: strings.Repeat("b", 64), InputSHA256: strings.Repeat("c", 64)}
	root, err := remotedata.Open(2, owner.Root, owner.RootID, req.TaskID, remotedata.Inputs, true)
	must(t, err)
	must(t, root.Mkdir("workspace", 0o700))
	must(t, root.Close())
	req.CanonicalCwd, err = remotedata.InputWorkspace(2, owner.Root, owner.RootID, req.TaskID)
	must(t, err)
	td, err := owner.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	return owner, req
}

func TestRemoteUseProtectsPreparationAndRegistersBeforeConsumer(t *testing.T) {
	owner, request := remoteUseOwner(t)
	consumer := testStore(t)
	req, _, _ := preparedInput(t, consumer)
	req.CanonicalCwd = request.CanonicalCwd
	use, err := consumer.BeginRemoteDataUse(t.Context(), *req)
	must(t, err)
	if use == nil {
		t.Fatal("managed cwd was not protected")
	}
	t.Cleanup(func() { must(t, use.Close()) })
	if lease, e := owner.AcquireRemoteByteUse(t.Context(), request.TaskID, false, true); !errors.Is(e, task.ErrLockBusy) {
		if lease != nil {
			must(t, lease.Close())
		}
		t.Fatalf("exclusive lease passed preparation: %v", e)
	}
	must(t, use.Register(t.Context()))
	must(t, use.Register(t.Context()))
	must(t, use.Close())
	journal, err := owner.OpenRemoteInputJournalContext(t.Context(), request.TaskID, false)
	must(t, err)
	defer func() { must(t, journal.Close()) }()
	names, err := journal.RecordNamesContext(t.Context())
	must(t, err)
	count := 0
	for _, name := range names {
		if strings.HasPrefix(name, "use-") {
			count++
			if len(name) != 61 {
				t.Fatal("invalid record name length", name)
			}
			var record RemoteUseRecord
			must(t, journal.Read(name, &record))
			if record.Root != consumer.Root || record.RootID != consumer.RootID || record.TaskID != req.TaskID || record.Cwd != req.CanonicalCwd {
				t.Fatalf("wrong use: %+v", record)
			}
		}
	}
	if count != 1 {
		t.Fatal("non-idempotent use count", count)
	}
	lease, err := owner.AcquireRemoteByteUse(t.Context(), request.TaskID, false, true)
	must(t, err)
	must(t, lease.Close())
}

func TestRemoteUsePreparationRefusalLeavesNoUseAndRetirementFencesCreation(t *testing.T) {
	owner, request := remoteUseOwner(t)
	consumer := testStore(t)
	req, _, _ := preparedInput(t, consumer)
	req.CanonicalCwd = request.CanonicalCwd
	use, err := consumer.BeginRemoteDataUse(t.Context(), *req)
	must(t, err)
	must(t, use.Close())
	journal, err := owner.OpenRemoteInputJournalContext(t.Context(), request.TaskID, false)
	must(t, err)
	defer func() { must(t, journal.Close()) }()
	names, err := journal.RecordNamesContext(t.Context())
	must(t, err)
	for _, name := range names {
		if strings.HasPrefix(name, "use-") {
			t.Fatal("preparation-only use persisted")
		}
	}
	_, err = journal.PutContext(t.Context(), "retirement.json", struct {
		Version int `json:"version"`
	}{1})
	must(t, err)
	if use, e := consumer.BeginRemoteDataUse(t.Context(), *req); !errors.Is(e, ErrRemoteDataRetired) || use != nil {
		if use != nil {
			must(t, use.Close())
		}
		t.Fatalf("retired data acquired: %v", e)
	}
}

func TestRemoteUseRejectsUnboundNamespaceAndPreservesUnmanagedCwd(t *testing.T) {
	owner := testStore(t)
	consumer := testStore(t)
	req, _, _ := preparedInput(t, consumer)
	req.CanonicalCwd = filepath.Join(filepath.Dir(owner.Root), ".state.remote-data", owner.RootID, strings.Repeat("a", 32), "inputs", "workspace")
	if use, err := consumer.BeginRemoteDataUse(t.Context(), *req); err == nil || use != nil {
		t.Fatal("unbound owner adopted")
	}
	req.CanonicalCwd = t.TempDir()
	use, err := consumer.BeginRemoteDataUse(t.Context(), *req)
	must(t, err)
	if use != nil {
		t.Fatal("unmanaged cwd got a use")
	}
}

func TestRemoteByteLeaseSerializesUploadAndRetirement(t *testing.T) {
	s := testStore(t)
	id := strings.Repeat("c", 32)
	shared, err := s.AcquireRemoteByteUse(t.Context(), id, true, false)
	must(t, err)
	if _, err = s.AcquireRemoteByteUse(t.Context(), id, true, true); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("active upload did not block %v", err)
	}
	must(t, shared.Close())
	exclusive, err := s.AcquireRemoteByteUse(t.Context(), id, true, true)
	must(t, err)
	must(t, exclusive.Close())
	if _, err = os.Stat(filepath.Join(s.Root, "remote-refs", id, ".byte-use.lock")); err != nil {
		t.Fatal("lease identity was unlinked", err)
	}
}

func TestRemoteByteLeaseProcessExitReleasesOwnership(t *testing.T) {
	if os.Getenv("DELEGATE_REMOTE_BYTE_CHILD") == "1" {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		owner, err := OpenStoreWithPredicatesContext(ctx, os.Getenv("DELEGATE_REMOTE_BYTE_ROOT"), predicate.Default())
		if err != nil {
			os.Exit(2)
		}
		if _, err = owner.AcquireRemoteByteUse(ctx, strings.Repeat("a", 32), true, false); err != nil {
			os.Exit(3)
		}
		if _, err = fmt.Fprint(os.Stdout, "locked"); err != nil {
			os.Exit(4)
		}
		if _, err = io.Copy(io.Discard, os.Stdin); err != nil {
			os.Exit(5)
		}
		os.Exit(0) // Deliberately bypass cleanup to prove kernel release for this owned fixture.
	}
	owner := testStore(t)
	command := exec.Command(os.Args[0], "-test.run=^TestRemoteByteLeaseProcessExitReleasesOwnership$")
	command.Env = append(os.Environ(), "DELEGATE_REMOTE_BYTE_CHILD=1", "DELEGATE_REMOTE_BYTE_ROOT="+owner.Root)
	input, err := command.StdinPipe()
	must(t, err)
	output, err := command.StdoutPipe()
	must(t, err)
	if err = command.Start(); err != nil {
		must(t, input.Close())
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			must(t, input.Close())
			if waitErr := command.Wait(); waitErr != nil {
				t.Error(waitErr)
			}
		}
	})
	ready := make([]byte, 6)
	_, err = io.ReadFull(output, ready)
	must(t, err)
	if string(ready) != "locked" {
		t.Fatal("missing ownership acknowledgement")
	}
	if _, err = owner.AcquireRemoteByteUse(t.Context(), strings.Repeat("a", 32), true, true); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("child lease not held: %v", err)
	}
	must(t, input.Close())
	err = command.Wait()
	waited = true
	must(t, err)
	lease, err := owner.AcquireRemoteByteUse(t.Context(), strings.Repeat("a", 32), true, true)
	must(t, err)
	must(t, lease.Close())
}
