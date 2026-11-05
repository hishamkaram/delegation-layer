package infrastructure

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type initialContactCase struct {
	name, mutation                                                     string
	code                                                               ownerReplyCode
	initial                                                            InitialOperationKind
	fresh, unclassified, wrongOperation, wrongContext, explicitDefault bool
	valid                                                              bool
}

func TestOwnerInitialFailureEndpointAfterReply(t *testing.T) {
	for _, row := range []initialContactCase{
		{name: "absent-fault", mutation: "absent", code: ownerFault, valid: true},
		{name: "absent-busy", mutation: "absent", code: ownerBusy, valid: true},
		{name: "absent-incompatible", mutation: "absent", code: ownerIncompatible, valid: true},
		{name: "absent-timeout", mutation: "absent", code: ownerTimeout, valid: true},
		{name: "absent-migration-completed", mutation: "absent", code: ownerFault, initial: InitialMigrateReferenced, valid: true},
		{name: "absent-manual-report", mutation: "absent", code: ownerFault, initial: InitialMigrateOwn, valid: true},
		{name: "live-same-socket", code: ownerFault, valid: true},
		{name: "explicit-default", mutation: "absent", code: ownerFault, explicitDefault: true, valid: true},
		{name: "success-strict", mutation: "absent", code: ownerOK, unclassified: true},
		{name: "pending-strict", mutation: "absent", code: ownerPending, unclassified: true},
		{name: "refused-strict", mutation: "absent", code: ownerRefused, unclassified: true},
		{name: "unclassified-strict", mutation: "absent", code: ownerFault, unclassified: true},
		{name: "fresh-strict", mutation: "absent", code: ownerFault, fresh: true},
		{name: "operation-mismatch-strict", mutation: "absent", code: ownerFault, wrongOperation: true},
		{name: "context-mismatch-strict", mutation: "absent", code: ownerFault, wrongContext: true},
		{name: "replacement-socket", mutation: "socket", code: ownerFault},
		{name: "replacement-file", mutation: "file", code: ownerFault},
		{name: "same-socket-wrong-mode", mutation: "mode", code: ownerFault},
		{name: "changed-owner", mutation: "owner", code: ownerFault},
		{name: "changed-publication", mutation: "publication", code: ownerFault},
		{name: "root-replaced", mutation: "root", code: ownerFault},
	} {
		t.Run(row.name, func(t *testing.T) { exerciseInitialContactCase(t, row) })
	}
}

func exerciseInitialContactCase(t *testing.T, row initialContactCase) {
	t.Helper()
	f := ownerContactFixtureWithSelection(t, false, row.initial)
	request, reply := initialFailureContactFrames(t, f, row)
	received := make(chan struct{}, 1)
	proceed := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(proceed) }) }
	var frames atomic.Int32
	joinServer := f.serve(t, func(connection *net.UnixConn, actual ownerRequest) error {
		frames.Add(1)
		if actual != request {
			return ErrIdentity
		}
		received <- struct{}{}
		<-proceed
		return writeOwnerFrame(connection, reply)
	})
	done := make(chan struct{})
	var got ownerReply
	var contactErr error
	var joinOnce sync.Once
	joinClient := func() {
		joinOnce.Do(func() {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("contact client did not join")
			}
		})
	}
	t.Cleanup(func() { release(); joinClient() })
	go func() { got, contactErr = contactOwner(t.Context(), f.reader, f.journal.pin, request); close(done) }()
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("native request never reached reply mutation boundary")
	}
	mutateInitialContactEndpoint(t, f, row.mutation)
	release()
	joinClient()
	if err := joinServer(); err != nil {
		t.Fatal(err)
	}
	if frames.Load() != 1 {
		t.Fatalf("contact sent %d frames", frames.Load())
	}
	if row.valid {
		if contactErr != nil || got != reply {
			t.Fatalf("classified initial failure lost: got=%v err=%v", got, contactErr)
		}
	} else {
		if contactErr == nil || got != (ownerReply{}) {
			t.Fatalf("stale endpoint/subject accepted: got=%v err=%v", got, contactErr)
		}
		requireOwnerContactPhase(t, contactErr, ownerContactMayHaveWritten, true)
	}
}

func initialFailureContactFrames(t *testing.T, f ownerContactFixture, row initialContactCase) (ownerRequest, ownerReply) {
	t.Helper()
	request := f.auth.request
	request.Action = preparationAction(f.auth.intent.Inputs.Initial)
	request.Source, request.Migration = f.auth.intent.Inputs.Initial.Source, f.auth.intent.Inputs.Initial.Migration
	if !row.fresh {
		request.InitialResultFor = request.Owner
	}
	if row.explicitDefault {
		request.Context = f.auth.intent.Inputs.Context
	}
	if row.wrongOperation {
		request.Action = ownerRecoverSaved
		request.Source = SavedBindingSource{RootID: request.RootID, Kind: SavedBindingTaskMetadata, OwnerID: strings.Repeat("b", 32), BindingSHA256: ownerWireDigest}
	}
	if row.wrongContext {
		selected := contextForJournal(t, f.journal)
		selected.ControlEnvironment = []string{"CONTEXT=other"}
		reference, _, err := f.journal.stageContext(t.Context(), selected)
		if err != nil {
			t.Fatal(err)
		}
		request.Context = reference
	}
	reply := ownerServerReply(request, row.code)
	reply.Target = originRecord{Root: f.journal.pin.Root(), RootID: f.journal.pin.RootID()}
	reply.Startup = request.Owner
	if !row.unclassified {
		reply.Failure = PreparationFailure{IndependentFault: true}
	}
	if row.code == ownerBusy {
		reply.Failure = PreparationFailure{Causes: PreparationCauseLockBusy}
	}
	if row.code == ownerOK {
		reply.Disposition = Disposition{Kind: DispositionWarmReuse}
	}
	if row.initial == InitialMigrateReferenced {
		reply.MigrationResult = MigrationCompleted
	}
	if row.initial == InitialMigrateOwn {
		reply.Report = ManualMigrationReport{State: ManualMigrationMigrated, Legacy: ManualMigrationLegacyRecovered, Active: ManualMigrationActiveNone}
	}
	return request, reply
}

func mutateInitialContactEndpoint(t *testing.T, f ownerContactFixture, mutation string) {
	t.Helper()
	path := f.auth.publication.Endpoint.Path
	switch mutation {
	case "":
		return
	case "absent":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	case "socket":
		replaceOwnerContactSocket(t, path)
	case "file":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("owned replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "mode":
		if err := os.Chmod(path, 0o660); err != nil {
			t.Fatal(err)
		}
	case "owner":
		mutateInitialOwnerHead(t, f)
	case "publication":
		mutateInitialPublication(t, f)
	case "root":
		replaceInitialContactRoot(t, f)

	default:
		t.Fatal("unknown mutation")
	}
}

func mutateInitialOwnerHead(t *testing.T, f ownerContactFixture) {
	t.Helper()
	next := f.auth.intent
	next.Sequence++
	next.Epoch = strings.Repeat("e", 32)
	if _, err := f.journal.putIntent(t.Context(), f.lease, next); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.auth.publication.Endpoint.Path); err != nil {
		t.Fatal(err)
	}
}

func mutateInitialPublication(t *testing.T, f ownerContactFixture) {
	t.Helper()
	next := f.auth.publication
	next.Endpoint.SocketInode++
	data, err := task.MarshalCanonical(next)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.journal.pin.Root(), "infrastructure", publicationName(next.Startup.Sequence)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(f.auth.publication.Endpoint.Path); err != nil {
		t.Fatal(err)
	}
}

func replaceInitialContactRoot(t *testing.T, f ownerContactFixture) {
	t.Helper()
	moved := f.auth.root + "-moved"
	if err := os.Rename(f.auth.root, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(f.auth.root); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
			return
		}
		if err := os.Rename(moved, f.auth.root); err != nil {
			t.Error(err)
		}
	})
	if err := os.Mkdir(f.auth.root, 0o700); err != nil {
		t.Fatal(err)
	}
}
