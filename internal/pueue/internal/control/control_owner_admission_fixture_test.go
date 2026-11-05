package control_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	inspectioncontract "github.com/hishamkaram/delegation-layer/internal/inspection/contract"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"golang.org/x/sys/unix"
)

// This external-package actor exercises the actual public consumer and journals.
// Root/native process observations and fault signals belong to lifetime tests.
func TestAdmissionControlCallerFixture(t *testing.T) {
	if os.Getenv("DELEGATE_ADMISSION_LEASE_CALLER") != "1" {
		t.Skip("fixed native admission caller")
	}
	deadline, err := strconv.ParseInt(os.Getenv("DELEGATE_ADMISSION_LEASE_DEADLINE"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(0, deadline))
	defer cancel()
	settings := admissionCallerSettings{root: os.Getenv("DELEGATE_ADMISSION_LEASE_DIRECTORY"), binary: os.Getenv("DELEGATE_ADMISSION_LEASE_PUEUE"), owner: os.Getenv("DELEGATE_ADMISSION_LEASE_OWNER"), role: os.Getenv("DELEGATE_ADMISSION_LEASE_ROLE"), workingDirectory: os.Getenv("DELEGATE_ADMISSION_LEASE_WORKSPACE")}
	if err = settings.validate(); err != nil {
		t.Fatal(err)
	}
	if err = settings.run(ctx); err != nil {
		t.Fatal(err)
	}
}

type (
	admissionCallerSettings struct{ root, binary, owner, role, workingDirectory string }
	admissionCallerSubject  struct {
		RootID string `json:"root_id"`
		TaskID string `json:"task_id"`
		Group  string `json:"group"`
		Label  string `json:"label"`
	}
	admissionCallerIdentity struct {
		PID     int    `json:"pid"`
		Token   string `json:"token"`
		Command string `json:"command"`
	}
)

func (s admissionCallerSettings) validate() error {
	for _, path := range []string{s.root, s.binary, s.owner, s.workingDirectory} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("native admission fixture requires absolute clean paths")
		}
	}
	switch s.role {
	case "ordinary-add", "inspection-add", "workspace-add", "ordinary-enqueue", "inspection-enqueue", "workspace-enqueue", "ordinary-recovery", "inspection-recovery", "workspace-recovery":
		return nil
	default:
		return errors.New("unknown fixed admission fixture role")
	}
}

func (s admissionCallerSettings) run(ctx context.Context) (resultErr error) {
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, s.root, predicate.Default())
	if err != nil {
		return err
	}
	lease, err := store.AcquireRootAdmissionContext(ctx)
	nativeClosed := true // No command exists before Bind.
	var nativeCloseErr error
	defer func() {
		releaseErr := lease.Release()
		resultErr = errors.Join(resultErr, releaseErr)
		if releaseErr == nil {
			storeErr := store.Close()
			resultErr = errors.Join(resultErr, storeErr)
			if nativeClosed && errors.Join(nativeCloseErr, storeErr) == nil {
				resultErr = errors.Join(resultErr, admissionCallerPublish(filepath.Join(s.root, ".supervisor", "caller-native-closed"), []byte("owned command Waits closed\n")))
			}
		}
	}()
	if err != nil {
		return err
	}
	ctx = pueue.WithControlLease(ctx, lease)
	base := filepath.Join(store.Root, ".supervisor")
	files, err := admissionCallerFiles(base)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := admissionCallerCloseFiles(files)
		nativeCloseErr = errors.Join(nativeCloseErr, closeErr)
		resultErr = errors.Join(resultErr, closeErr)
	}()
	ctx = pueue.WithControlLease(ctx, admissionCallerLease{files: files})
	config := filepath.Join(base, "operator.yml")
	if strings.HasPrefix(s.role, "workspace-") {
		config = pueue.PrivateConfigPath(store.Root)
	}
	if err = pueue.WithBootstrapLockContext(ctx, store.Root, func(context.Context) error { return nil }); err != nil {
		return err
	}
	started := make(chan error, 1)
	nativeClosed = false
	client, err := pueue.Bind(ctx, s.binary, config, pueue.Options{Observer: admissionCallerObserver(base, s.role, started), ControlOwnerExecutable: s.owner, ObservationTimeout: 5 * time.Second, WaitForPendingClose: true, Environment: []string{"HOME=" + store.Root, "PATH=/usr/bin:/bin", "LANG=C"}})
	if err != nil {
		return errors.Join(err, client.CloseAfterPending(err))
	}
	defer func() {
		closeErr := client.CloseAfterPending(resultErr)
		nativeClosed = closeErr == nil
		nativeCloseErr = errors.Join(nativeCloseErr, closeErr)
		resultErr = errors.Join(resultErr, closeErr)
	}()
	if strings.HasPrefix(s.role, "workspace-") {
		return s.runAction(ctx, store, client, started)
	}
	return pueue.WithBootstrapLockContext(ctx, store.Root, func(locked context.Context) error { return s.runAction(locked, store, client, started) })
}

func (s admissionCallerSettings) runAction(ctx context.Context, store *taskdir.Store, client *pueue.Client, started <-chan error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	base := filepath.Join(store.Root, ".supervisor")
	var err error
	completed := make(chan error, 1)
	go func() { completed <- s.submit(ctx, store, client) }()
	select {
	case err = <-started:
		if err != nil {
			cancel()
			return errors.Join(err, <-completed)
		}
	case err = <-completed:
		return errors.Join(errors.New("admission command ended before target Start"), err)
	case <-ctx.Done():
		return errors.Join(ctx.Err(), <-completed)
	}
	if err = admissionCallerRelease(ctx, base); err != nil {
		cancel()
		return errors.Join(err, <-completed)
	}
	// Intentional creator loss AFTER target identity handoff. Every transferred
	// holder closes only; the external owner retains the actual command Wait.
	os.Exit(0)
	return nil
}

func admissionCallerObserver(base, role string, started chan<- error) func(pueue.CommandEvent) {
	return func(event pueue.CommandEvent) {
		if event.Stage != "started" || len(event.Argv) < 4 {
			return
		}
		command := event.Argv[3]
		if command != "status" && command != "add" && command != "enqueue" {
			return
		}
		target := admissionCallerTarget(role, command)
		err := admissionCallerPublishProcess(base, command, event.PID, target)
		if target || err != nil {
			select {
			case started <- err:
			default:
			}
		}
	}
}

func admissionCallerTarget(role, command string) bool {
	if strings.HasSuffix(role, "-add") {
		return command == "add"
	}
	return command == "enqueue"
}

func admissionCallerPublishProcess(base, command string, pid int, target bool) error {
	ref, err := lifetime.ObserveProcess(pid)
	if err != nil {
		return err
	}
	data, err := task.MarshalCanonical(admissionCallerIdentity{PID: ref.PID(), Token: ref.StartToken(), Command: command})
	if err != nil {
		return err
	}
	if err = admissionCallerPublish(filepath.Join(base, "active-cli.json"), data); err != nil {
		return err
	}
	if target {
		return admissionCallerPublish(filepath.Join(base, "cli.json"), data)
	}
	return nil
}

func TestAdmissionCallerObserverReportsSetupPublicationFault(t *testing.T) {
	started := make(chan error, 1)
	observe := admissionCallerObserver(filepath.Join(t.TempDir(), "missing"), "ordinary-enqueue", started)
	observe(pueue.CommandEvent{Stage: "started", PID: os.Getpid(), Argv: []string{"pueue", "-c", "operator.yml", "status"}})
	select {
	case err := <-started:
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("setup publication lost filesystem cause: %v", err)
		}
	default:
		t.Fatal("non-target native setup publication fault was swallowed")
	}
}

func (s admissionCallerSettings) submit(ctx context.Context, store *taskdir.Store, client *pueue.Client) error {
	request, meta, err := s.taskRecords(store, client.Binding())
	if err != nil {
		return err
	}
	switch {
	case strings.HasPrefix(s.role, "ordinary-"):
		return s.ordinary(ctx, store, client, request, meta)
	case strings.HasPrefix(s.role, "inspection-"):
		return s.inspection(ctx, store, client, request)
	case strings.HasPrefix(s.role, "workspace-"):
		return s.workspace(ctx, store, client, request)
	default:
		return errors.New("unknown admission subject")
	}
}

func (s admissionCallerSettings) taskRecords(store *taskdir.Store, supervisor task.SupervisorRef) (task.TaskRecord, task.MetaRecord, error) {
	id, err := randomid.NewTaskID()
	if err != nil {
		return task.TaskRecord{}, task.MetaRecord{}, err
	}
	config := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	brief := []byte("native admission actuator fixture")
	request := task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: id, Provider: "fixture:test", Mode: "read-only", CanonicalCwd: s.workingDirectory, RequestedConfig: config, BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief))}
	meta := task.MetaRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: id, RequestedConfig: config, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("native fixture config"))}, Containment: "fixture-only", Approval: "never", ProviderExecutable: s.owner, ProviderVersion: "native-fixture", PublisherBuild: "native-fixture", PublisherVersion: "test", Predicate: task.FixturePredicateRef(), SupervisorConfig: supervisor, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	return request, meta, nil
}

var errAdmissionFixtureReceipt = errors.New("fixed fixture stops after durable receipt")

func (s admissionCallerSettings) ordinary(ctx context.Context, store *taskdir.Store, client *pueue.Client, request task.TaskRecord, meta task.MetaRecord) (resultErr error) {
	directory, err := store.CreateTaskContext(ctx, request.TaskID, &request, []byte("native admission actuator fixture"), &meta)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	permit, err := directory.PrepareSubmissionContext(ctx, client.Binding())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, permit.Release()) }()
	record, err := permit.Record()
	if err != nil {
		return err
	}
	identity := pueue.Identity{RootID: record.RootID, TaskID: record.TaskID, SpecSHA256: record.SpecSHA256, MetaSHA256: record.MetaSHA256}
	if err = admissionCallerSubjectRecord(store.Root, admissionCallerSubject{RootID: identity.RootID, TaskID: identity.TaskID, Group: "default", Label: identity.Label()}); err != nil {
		return err
	}
	commit := func(candidate pueue.Observation) error {
		receipt, receiptErr := candidate.Receipt()
		if receiptErr != nil {
			return receiptErr
		}
		if receiptErr = directory.RecordSupervisorReceiptContext(ctx, *receipt); receiptErr != nil {
			return receiptErr
		}
		if strings.HasSuffix(s.role, "-recovery") {
			return errAdmissionFixtureReceipt
		}
		return nil
	}
	_, err = client.SubmitStaged(ctx, permit, pueue.Launch{RunnerExecutable: s.owner, RootPath: store.Root, Supervised: true}, commit)
	if !strings.HasSuffix(s.role, "-recovery") {
		return err
	}
	if !errors.Is(err, errAdmissionFixtureReceipt) {
		return errors.Join(errors.New("setup add did not stop at actual receipt"), err)
	}
	_, err = client.RecoverStaged(ctx, identity, 7, func(candidate pueue.Observation) error {
		receipt, receiptErr := candidate.Receipt()
		if receiptErr != nil {
			return receiptErr
		}
		return directory.RecordSupervisorReceiptContext(ctx, *receipt)
	})
	return err
}

func (s admissionCallerSettings) inspection(ctx context.Context, store *taskdir.Store, client *pueue.Client, request task.TaskRecord) (resultErr error) {
	image, err := os.ReadFile(s.owner)
	if err != nil {
		return err
	}
	digest := task.ComputeSHA256(image)
	operation, err := inspection.OpenOperationContext(ctx, store, request, inspectioncontract.Binding{DefinitionRevision: "native-actuator/v1", DefinitionSHA256: task.ComputeSHA256([]byte("native actuator")), HelperExecutable: s.owner, HelperSHA256: digest, WorkerExecutable: s.owner, WorkerSHA256: digest, Supervisor: client.Binding()}, time.Now())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	permit, err := operation.ClaimSubmissionContext(ctx, time.Now())
	if err != nil {
		return err
	}
	identity := operation.Request().SupervisorIdentity()
	return s.auxiliary(ctx, store, client, permit, identity, pueue.Launch{RunnerExecutable: s.owner, RootPath: store.Root, Supervised: true}, func(id int64) error { return operation.RecordReceiptContext(ctx, id) })
}

func (s admissionCallerSettings) workspace(ctx context.Context, store *taskdir.Store, client *pueue.Client, request task.TaskRecord) (resultErr error) {
	image, err := os.ReadFile(s.owner)
	if err != nil {
		return err
	}
	workspaceID, err := randomid.NewTaskID()
	if err != nil {
		return err
	}
	operation, err := workspace.OpenOperation(ctx, store, workspace.OperationRequest{SchemaVersion: task.SchemaVersion, RootID: store.RootID, WorkspaceID: workspaceID, OperationID: request.TaskID, Kind: workspace.OperationProvision, Subject: workspace.Subject{Kind: workspace.SubjectAdmission, ID: request.TaskID, SHA256: task.ComputeSHA256([]byte("native admission"))}, Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano), WorkerExecutable: s.owner, WorkerSHA256: task.ComputeSHA256(image), Supervisor: client.Binding()})
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	permit, err := operation.SubmissionPermit(ctx)
	if err != nil {
		return err
	}
	identity := pueue.InspectionIdentity{RootID: store.RootID, TaskID: request.TaskID, QueueGroup: "default"}
	return s.auxiliary(ctx, store, client, permit, identity, pueue.Launch{RunnerExecutable: s.owner, RootPath: store.Root, Supervised: true, WorkspaceOperation: true}, func(id int64) error { return operation.RecordReceipt(ctx, id) })
}

func (s admissionCallerSettings) auxiliary(ctx context.Context, store *taskdir.Store, client *pueue.Client, permit pueue.InspectionSubmissionPermit, identity pueue.InspectionIdentity, launch pueue.Launch, record func(int64) error) error {
	if err := admissionCallerSubjectRecord(store.Root, admissionCallerSubject{RootID: identity.RootID, TaskID: identity.TaskID, Group: identity.Group(), Label: identity.Label()}); err != nil {
		return err
	}
	commit := func(candidate pueue.InspectionObservation) error {
		if err := record(candidate.Job.ID); err != nil {
			return err
		}
		if strings.HasSuffix(s.role, "-recovery") {
			return errAdmissionFixtureReceipt
		}
		return nil
	}
	var err error
	if launch.WorkspaceOperation {
		_, err = client.SubmitConcurrentInspectionStaged(ctx, permit, identity, launch, commit)
	} else {
		_, err = client.SubmitInspectionStaged(ctx, permit, identity, launch, commit)
	}
	if !strings.HasSuffix(s.role, "-recovery") {
		return err
	}
	if !errors.Is(err, errAdmissionFixtureReceipt) {
		return errors.Join(errors.New("setup aux add did not stop at actual receipt"), err)
	}
	_, err = client.RecoverInspectionStaged(ctx, identity, 7, func(candidate pueue.InspectionObservation) error { return record(candidate.Job.ID) })
	return err
}

func admissionCallerSubjectRecord(root string, subject admissionCallerSubject) error {
	data, err := task.MarshalCanonical(subject)
	if err != nil {
		return err
	}
	return admissionCallerPublish(filepath.Join(root, ".supervisor", "subject.json"), data)
}

func admissionCallerPublish(path string, data []byte) (resultErr error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".admission-record-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() {
		if removeErr := os.Remove(name); !errors.Is(removeErr, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, removeErr)
		}
	}()
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err = errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(name, path)
}

type admissionCallerLease struct{ files []*os.File }

func (l admissionCallerLease) DuplicateControlLeaseFiles() (files []*os.File, resultErr error) {
	for _, file := range l.files {
		fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			return nil, errors.Join(err, admissionCallerCloseFiles(files))
		}
		duplicate := os.NewFile(uintptr(fd), "admission-fixture-decoy")
		if duplicate == nil {
			return nil, errors.Join(os.ErrInvalid, unix.Close(fd), admissionCallerCloseFiles(files))
		}
		files = append(files, duplicate)
	}
	return files, nil
}

func admissionCallerCloseFiles(files []*os.File) (resultErr error) {
	for _, file := range files {
		resultErr = errors.Join(resultErr, file.Close())
	}
	return resultErr
}

func admissionCallerFiles(base string) (files []*os.File, resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, admissionCallerCloseFiles(files))
			files = nil
		}
	}()
	for _, name := range []string{"queue", "task-maintenance", "lifetime", "instance"} {
		file, err := os.OpenFile(filepath.Join(base, name+".lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return files, err
		}
		files = append(files, file)
		if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return files, err
		}
	}
	for _, fd := range []int{3, 4} {
		file := os.NewFile(uintptr(fd), "admission-caller-ipc")
		if file == nil {
			return files, os.ErrInvalid
		}
		files = append(files, file)
		flags, flagErr := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
		if flagErr != nil {
			return files, flagErr
		}
		if _, flagErr = unix.FcntlInt(file.Fd(), unix.F_SETFD, flags|unix.FD_CLOEXEC); flagErr != nil {
			return files, flagErr
		}
		flags, flagErr = unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
		if flagErr != nil {
			return files, flagErr
		}
		if flags&unix.FD_CLOEXEC == 0 {
			return files, errors.New("caller IPC CLOEXEC not established")
		}
	}
	return files, nil
}

func admissionCallerRelease(ctx context.Context, base string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(base, "abort")); err == nil {
			return errors.New("native fixture aborted")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := os.Stat(filepath.Join(base, "managed.release-caller")); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
