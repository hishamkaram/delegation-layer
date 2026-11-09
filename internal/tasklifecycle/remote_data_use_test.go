package tasklifecycle

import (
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

func TestRemoteDataUseCreationProtectsProviderPreparationAndRefusalLeavesNoUse(t *testing.T) {
	owner, req := remoteDataUseCreationOwner(t)
	consumer, _, consumerRequest := testsupport.NewAppTestTask(t, false)
	child := *consumerRequest
	child.TaskID, child.CanonicalCwd = strings.Repeat("b", 32), req.CanonicalCwd
	refused := errors.New("provider preparation refused")
	inspected := false
	candidates := teamResumeCandidates{prepare: func(task.TaskRecord, []string) (commonprovider.ProfileCandidate, error) {
		inspected = true
		lease, e := owner.AcquireRemoteByteUse(t.Context(), req.TaskID, false, true)
		if lease != nil {
			if closeErr := lease.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		if !errors.Is(e, task.ErrLockBusy) {
			t.Errorf("provider preparation was not protected: %v", e)
		}
		return commonprovider.ProfileCandidate{}, refused
	}}
	result := CreateTaskContext(t.Context(), supervisorcore.AdmissionSelection{}, CreationOptions{Admission: admissioncore.PreparationOptions{Profiles: admissioncore.NewProfiles(candidates, nil, []string{})}}, consumer, child, []byte("brief"), "")
	if !inspected || !errors.Is(result.Err(), refused) || result.TaskRecord != CreationRecordUnobserved {
		t.Fatalf("preparation result=%+v inspected=%v", result, inspected)
	}
	journal, err := owner.OpenRemoteInputJournalContext(t.Context(), req.TaskID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e := journal.Close(); e != nil {
			t.Error(e)
		}
	}()
	names, err := journal.RecordNamesContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "use-") {
			t.Fatalf("refused provider persisted consumer: %s", name)
		}
	}
	lease, err := owner.AcquireRemoteByteUse(t.Context(), req.TaskID, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func remoteDataUseCreationOwner(t *testing.T) (*taskdir.Store, task.TaskRecord) {
	t.Helper()
	owner, seed, original := testsupport.NewAppTestTask(t, false)
	_, meta, err := seed.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	req := *original
	req.TaskID = strings.Repeat("a", 32)
	req.Remote = &task.RemoteBinding{StorageLayout: task.RemoteDataOrganized, OriginRootID: strings.Repeat("b", 32), IntentSHA256: strings.Repeat("c", 64), InputSHA256: strings.Repeat("d", 64)}
	root, err := remotedata.Open(2, owner.Root, owner.RootID, req.TaskID, remotedata.Inputs, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(root.Mkdir("workspace", 0o700), root.Close()); err != nil {
		t.Fatal(err)
	}
	req.CanonicalCwd, err = remotedata.InputWorkspace(2, owner.Root, owner.RootID, req.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	meta.TaskID, meta.SpecSHA256 = req.TaskID, ""
	td, err := owner.CreateTask(req.TaskID, &req, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := td.Close(); e != nil {
			t.Error(e)
		}
	})

	return owner, req
}

type remoteUseCreationInterruption struct {
	taskdir.BaseFaultInjector
	target  string
	failure error
	fired   bool
}

func (f *remoteUseCreationInterruption) OnAfterLinkDirBarrier(destination string) error {
	if destination == f.target && !f.fired {
		f.fired = true
		return f.failure
	}
	return nil
}

func TestRemoteDataUseAmbiguousCreationRetainsDependency(t *testing.T) {
	owner, request := remoteDataUseCreationOwner(t)
	consumer, _, original := testsupport.NewAppTestTask(t, false)
	req := *original
	req.TaskID, req.CanonicalCwd = strings.Repeat("d", 32), request.CanonicalCwd
	base := supervisorfixture.CanonicalTempDir(t)
	supervisor, configPath := schedulerDispatchSupervisor(t, base)
	dispatch := schedulerDispatchRequest(t, consumer.Root, configPath, 0)
	dispatch.TaskID = req.TaskID
	preparation := newSchedulerDispatchGate(t.Context())
	preparation.remaining.Store(1)
	var adds atomic.Int32
	options := schedulerDispatchOptions(dispatch, supervisor, preparation, nil, errors.New("submission must not begin"), &adds)
	injected := errors.New("interrupted after durable task record")
	fault := &remoteUseCreationInterruption{target: filepath.Join(consumer.Root, "tasks", req.TaskID, "task.json"), failure: injected}
	consumer.SetFaultInjector(fault)
	result := CreateTaskContext(t.Context(), dispatch.Selection, options.Creation, consumer, req, []byte("brief"), "")
	consumer.SetFaultInjector(nil)
	if !fault.fired || !errors.Is(result.Err(), injected) || result.TaskRecord != CreationRecordUnknown || result.Submission != nil {
		t.Fatalf("ambiguous creation=%+v fired=%v error=%v", result, fault.fired, result.Err())
	}
	if adds.Load() != 0 {
		t.Fatal("ambiguous creation submitted queue work")
	}
	journal, err := owner.OpenRemoteInputJournalContext(t.Context(), request.TaskID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e := journal.Close(); e != nil {
			t.Error(e)
		}
	}()
	use := taskdir.RemoteUseRecord{Root: consumer.Root, RootID: consumer.RootID, TaskID: req.TaskID, Cwd: req.CanonicalCwd}
	name, err := taskdir.RemoteUseName(use)
	if err != nil {
		t.Fatal(err)
	}
	var recorded taskdir.RemoteUseRecord
	if err = journal.Read(name, &recorded); err != nil || recorded != use {
		t.Fatalf("ambiguous use lost: %+v %v", recorded, err)
	}
	lease, err := owner.AcquireRemoteByteUse(t.Context(), request.TaskID, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
}
