package infrastructure

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This same-process socket fixture proves authentication component observations,
// not a separate resident, trusted native birth, or managed-image authority.
type ownerAuthFixture struct {
	root        string
	store       *taskdir.Store
	listener    *net.UnixListener
	client      *net.UnixConn
	server      *net.UnixConn
	intent      StartupIntent
	assumption  StartupAssumption
	publication OwnerPublication
	request     ownerRequest
}

func newOwnerAuthFixture(t *testing.T) *ownerAuthFixture {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".oa-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &ownerAuthFixture{root: root}
	t.Cleanup(func() {
		if closeErr := fixture.close(); closeErr != nil {
			t.Errorf("retained authentication fixture %s: %v", root, closeErr)
			return
		}
		if removeErr := os.RemoveAll(root); removeErr != nil {
			t.Error(removeErr)
		}
	})
	fixture.store, err = taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	fixture.connect(t)
	fixture.records(t)
	return fixture
}

func (f *ownerAuthFixture) connect(t *testing.T) {
	t.Helper()
	address := &net.UnixAddr{Name: ownerEndpointPath(f.root, 1), Net: "unix"}
	var err error
	f.listener, err = net.ListenUnix("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	f.client, err = net.DialUnix("unix", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	f.server, err = f.listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *ownerAuthFixture) close() error {
	var result error
	for _, connection := range []*net.UnixConn{f.client, f.server} {
		if connection != nil {
			if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				result = errors.Join(result, err)
			}
		}
	}
	if f.listener != nil {
		if err := f.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, err)
		}
	}
	if f.store != nil {
		result = errors.Join(result, f.store.Close())
	}
	return result
}

func (f *ownerAuthFixture) records(t *testing.T) {
	t.Helper()
	pin, err := f.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	bootID, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	process, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	f.intent = startupIntentFixture(t)
	f.intent.Inputs.Root, f.intent.Inputs.RootID = pin.Root(), pin.RootID()
	f.intent.Inputs.RootDevice, f.intent.Inputs.RootInode = pin.Device(), pin.Inode()
	f.intent.Inputs.BootID = bootID
	// A legitimate prior assumption remains authentic after its original deadline.
	now := time.Now().UTC()
	f.intent.AcceptedAt = now.Add(-3 * time.Nanosecond).Format(time.RFC3339Nano)
	f.intent.Deadline = now.Add(-time.Nanosecond).Format(time.RFC3339Nano)
	refreshIntentDigest(t, &f.intent)
	reference, err := ReferenceStartup(f.intent)
	if err != nil {
		t.Fatal(err)
	}
	f.assumption = StartupAssumption{Startup: reference, Process: ProcessIdentity{PID: process.PID(), StartToken: process.StartToken()}, UID: os.Geteuid(), AssumedAt: now.Add(-2 * time.Nanosecond).Format(time.RFC3339Nano)}
	socket, err := observeEndpointSocket(ownerEndpointPath(f.root, 1))
	if err != nil {
		t.Fatal(err)
	}
	f.publication = OwnerPublication{Protocol: RecordProtocolVersion, Startup: reference, Endpoint: EndpointIdentity{Path: ownerEndpointPath(f.root, 1), DirectoryDev: pin.Device(), DirectoryInode: pin.Inode(), SocketDev: uint64(socket.Dev), SocketInode: uint64(socket.Ino)}}
	f.rebind(t)
	f.request = ownerRequest{Protocol: ownerProtocolVersion, RootID: pin.RootID(), Owner: reference, Client: f.assumption.Process, Action: ownerStatus, Deadline: now.Add(time.Minute).Format(time.RFC3339Nano)}
	digest, err := f.publication.Digest(f.intent, f.assumption)
	if err != nil {
		t.Fatal(err)
	}
	f.request.PublicationSHA256 = digest
}

func (f *ownerAuthFixture) rebind(t *testing.T) {
	t.Helper()
	refreshIntentDigest(t, &f.intent)
	reference, err := ReferenceStartup(f.intent)
	if err != nil {
		t.Fatal(err)
	}
	f.assumption.Startup, f.publication.Startup = reference, reference
	digest, err := f.assumption.Digest(f.intent)
	if err != nil {
		t.Fatal(err)
	}
	f.publication.AssumptionSHA256 = digest
	digest, err = f.publication.Digest(f.intent, f.assumption)
	if err != nil {
		t.Fatal(err)
	}
	f.request.Owner, f.request.PublicationSHA256 = reference, digest
}

func TestOwnerAuthenticationAcceptsActualPeerAfterStartupDeadline(t *testing.T) {
	fixture := newOwnerAuthFixture(t)
	expired, err := fixture.intent.Expired(time.Now())
	if err != nil || !expired {
		t.Fatalf("fixture must be historical startup: %v", err)
	}
	if err = authenticateOwnerRequest(t.Context(), fixture.server, fixture.intent, fixture.assumption, fixture.publication, fixture.request); err != nil {
		t.Fatal(err)
	}
	if err = authenticateOwnerPeer(t.Context(), fixture.client, fixture.intent, fixture.assumption, fixture.publication); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerAuthenticationRefusesEnsureOtherForSameOwner(t *testing.T) {
	fixture := newOwnerAuthFixture(t)
	request := fixture.request
	request.Action = ownerEnsureOther
	request.Context = LaunchContextReference{SHA256: strings.Repeat("d", 64)}
	request.Target = originRecord{Root: filepath.Join(fixture.root, "other"), RootID: strings.Repeat("e", 32)}
	if err := authenticateOwnerRequest(t.Context(), fixture.server, fixture.intent, fixture.assumption, fixture.publication, request); err != nil {
		t.Fatalf("valid other-root request failed native authentication: %v", err)
	}
	request.Target = originRecord{Root: fixture.root, RootID: fixture.intent.Inputs.RootID}
	if err := authenticateOwnerRequest(t.Context(), fixture.server, fixture.intent, fixture.assumption, fixture.publication, request); !errors.Is(err, ErrIdentity) {
		t.Fatalf("authenticated owner accepted its own root as EnsureOther: %v", err)
	}
}

func TestOwnerAuthenticationRefusesChangedClientAndSubject(t *testing.T) {
	fixture := newOwnerAuthFixture(t)
	for _, kind := range []string{"pid", "token", "epoch", "namespace", "publication", "reference", "action", "target"} {
		changed := fixture.request
		switch kind {
		case "pid":
			changed.Client.PID++
		case "token":
			changed.Client = differentOwnerFixtureProcess(t, changed.Client)
		case "epoch":
			changed.Owner.Epoch = strings.Repeat("e", 32)
		case "namespace":
			changed.RootID = strings.Repeat("e", 32)
		case "publication":
			changed.PublicationSHA256 = strings.Repeat("e", 64)
		case "reference":
			changed.Owner.IntentSHA256 = strings.Repeat("e", 64)
		case "action":
			changed.Action = "unsupported"
		case "target":
			changed.Target = originRecord{Root: "/wrong", RootID: strings.Repeat("e", 32)}
		}
		requireOwnerAuthRefusal(t, authenticateOwnerRequest(t.Context(), fixture.server, fixture.intent, fixture.assumption, fixture.publication, changed))
	}
}

func requireOwnerAuthRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil || errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("authentication accepted/used evidence classification: %v", err)
	}
}

func TestOwnerAuthenticationRefusesChangedOwnerRecords(t *testing.T) {
	fixture := newOwnerAuthFixture(t)
	for _, kind := range []string{"pid", "token", "uid", "assumption-digest", "endpoint"} {
		assumption, publication, request := changedOwnerAuthenticationRecords(t, fixture, kind)
		requireOwnerAuthRefusal(t, authenticateOwnerPeer(t.Context(), fixture.client, fixture.intent, assumption, publication))
		requireOwnerAuthRefusal(t, authenticateOwnerRequest(t.Context(), fixture.server, fixture.intent, assumption, publication, request))
	}
}

func changedOwnerAuthenticationRecords(t *testing.T, fixture *ownerAuthFixture, kind string) (StartupAssumption, OwnerPublication, ownerRequest) {
	t.Helper()
	assumption, publication, request := fixture.assumption, fixture.publication, fixture.request
	switch kind {
	case "pid":
		assumption.Process.PID++
	case "token":
		assumption.Process = differentOwnerFixtureProcess(t, assumption.Process)
	case "uid":
		assumption.UID++
	case "assumption-digest":
		publication.AssumptionSHA256 = strings.Repeat("e", 64)
	case "endpoint":
		publication.Endpoint.Path = filepath.Join(fixture.root, "wrong")
	}
	if kind == "pid" || kind == "token" || kind == "uid" {
		digest, err := assumption.Digest(fixture.intent)
		if err != nil {
			t.Fatal(err)
		}
		publication.AssumptionSHA256 = digest
		digest, err = publication.Digest(fixture.intent, assumption)
		if err != nil {
			t.Fatal(err)
		}
		request.PublicationSHA256 = digest
	}
	return assumption, publication, request
}

func TestOwnerAuthenticationBootPrecedesKernelPeerObservation(t *testing.T) {
	fixture := newOwnerAuthFixture(t)
	fixture.intent.Inputs.BootID = otherStableBoot(t, fixture.intent.Inputs.BootID)
	fixture.rebind(t)
	if err := fixture.client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.server.Close(); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		authenticateOwnerPeer(t.Context(), fixture.client, fixture.intent, fixture.assumption, fixture.publication),
		authenticateOwnerRequest(t.Context(), fixture.server, fixture.intent, fixture.assumption, fixture.publication, fixture.request),
	} {
		if !errors.Is(err, ErrIdentity) || errors.Is(err, net.ErrClosed) {
			t.Fatalf("historical boot reached native peer PID query: %v", err)
		}
	}
}

func TestOwnerAuthenticationPreservesContextAndNativeErrors(t *testing.T) {
	fixture := newOwnerAuthFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := authenticateOwnerRequest(ctx, fixture.server, fixture.intent, fixture.assumption, fixture.publication, fixture.request); !errors.Is(err, context.Canceled) || errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("canceled authentication: %v", err)
	}
	if err := authenticateOwnerPeer(ctx, fixture.client, fixture.intent, fixture.assumption, fixture.publication); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled peer authentication: %v", err)
	}
	var absent context.Context
	if err := authenticateOwnerPeer(absent, fixture.client, fixture.intent, fixture.assumption, fixture.publication); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil context: %v", err)
	}
	if err := authenticateOwnerPeer(t.Context(), nil, fixture.intent, fixture.assumption, fixture.publication); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil peer: %v", err)
	}
	if err := fixture.client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := authenticateOwnerPeer(t.Context(), fixture.client, fixture.intent, fixture.assumption, fixture.publication); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("native close cause lost: %v", err)
	}
}

func TestOwnerReplyBindingKeepsExactActionAndTargetSubject(t *testing.T) {
	fixture := newOwnerAuthFixture(t)
	for _, action := range []ownerAction{ownerStatus, ownerEnsureOwn, ownerEnsureOther, ownerRecoverSaved, ownerPrepareContinuation} {
		request, reply := ownerReplySubjectFixture(fixture, action)
		if err := validateOwnerReplyBinding(fixture.intent, fixture.assumption, fixture.publication, request, reply); err != nil {
			t.Fatalf("valid %s binding: %v", action, err)
		}
		for _, kind := range []string{"root-id", "reference", "publication", "action", "target"} {
			changed := reply
			switch kind {
			case "root-id":
				changed.RootID = strings.Repeat("e", 32)
			case "reference":
				changed.OwnerRef.IntentSHA256 = strings.Repeat("e", 64)
			case "publication":
				changed.PublicationSHA256 = strings.Repeat("e", 64)
			case "action":
				changed.Action = ownerEnsureOther
				changed.Target = originRecord{Root: filepath.Join(fixture.root, "other"), RootID: strings.Repeat("e", 32)}
				changed.Startup = StartupReference{Sequence: 2, Epoch: strings.Repeat("d", 32), IntentSHA256: strings.Repeat("d", 64)}
				if action == ownerEnsureOther {
					changed.Action = ownerStatus
					changed.Target = originRecord{}
					changed.Startup = StartupReference{}
				}
			case "target":
				changed.Target.Root = filepath.Join(fixture.root, "different-target")
			}
			requireOwnerAuthRefusal(t, validateOwnerReplyBinding(fixture.intent, fixture.assumption, fixture.publication, request, changed))
		}
	}
}

func ownerReplySubjectFixture(fixture *ownerAuthFixture, action ownerAction) (ownerRequest, ownerReply) {
	request := fixture.request
	request.Action = action
	reply := ownerReply{Protocol: ownerProtocolVersion, RootID: request.RootID, OwnerRef: request.Owner, PublicationSHA256: request.PublicationSHA256, Action: action, Code: ownerOK}
	switch action {
	case ownerStatus:
	case ownerRuntimeRestartPreflight:
		request.Context = fixture.intent.Inputs.Context
		reply.Context = request.Context
		reply.RestartPreflight = RuntimeRestartPreflight{Protocol: runtimeRestartProtocolVersion, From: RuntimeRestartReference{RootID: request.RootID, Startup: request.Owner}, Custody: RuntimeRestartCustodyUnknown, ObservedAt: "2026-11-12T00:00:00Z"}
	case ownerRuntimeRestart:
		request.RestartFrom, reply.RestartFrom = request.Owner, request.Owner
		reply.Target = originRecord{Root: fixture.root, RootID: request.RootID}
		reply.Startup = request.Owner
		reply.RuntimeRestart = RuntimeRestartReport{From: request.Owner, Phase: RuntimeRestartDaemonClosed}
	case ownerRecoverSaved, ownerPrepareContinuation:
		selected := ownerPreparationSubjectFixture(action)
		request.Source, request.Context = selected.Source, selected.Context
		request.Source.RootID = request.RootID
		reply.Source, reply.Context = request.Source, request.Context
		reply.Target = originRecord{Root: fixture.root, RootID: request.RootID}
		reply.Startup = request.Owner
		reply.Disposition = Disposition{Kind: DispositionWarmReuse}
	case ownerEnsureOwn, ownerMigrateOwn:
		if action == ownerMigrateOwn {
			reply.Report = ManualMigrationReport{State: ManualMigrationNotInitialized, Legacy: ManualMigrationLegacyNotPresent, Active: ManualMigrationActiveNone}
		} else {
			reply.Disposition = Disposition{Kind: DispositionWarmReuse}
		}
		reply.Target = originRecord{Root: fixture.root, RootID: fixture.intent.Inputs.RootID}
		reply.Startup = request.Owner
	case ownerEnsureOther:
		request.Target = originRecord{Root: filepath.Join(fixture.root, "target"), RootID: strings.Repeat("e", 32)}
		request.Context = LaunchContextReference{SHA256: strings.Repeat("d", 64)}

		reply.Context = request.Context
		// Establishment returns B's startup only; preparation is a direct B RPC.
		reply.Target = request.Target
		reply.Startup = StartupReference{Sequence: 2, Epoch: strings.Repeat("d", 32), IntentSHA256: strings.Repeat("d", 64)}
	}
	return request, reply
}

func TestOwnerReplyBindingPermitsCanonicalOwnRefusalButNotWrongRoot(t *testing.T) {
	fixture := newOwnerAuthFixture(t)
	request, reply := ownerReplySubjectFixture(fixture, ownerEnsureOwn)
	reply.Code = ownerRefused
	reply.Target, reply.Startup = originRecord{}, StartupReference{}
	if err := validateOwnerReplyBinding(fixture.intent, fixture.assumption, fixture.publication, request, reply); err != nil {
		t.Fatalf("canonical own refusal: %v", err)
	}
	reply.Target = originRecord{Root: filepath.Join(fixture.root, "different"), RootID: fixture.intent.Inputs.RootID}
	if err := reply.validate(); err != nil {
		t.Fatalf("wrong-root fixture must be independently shape valid: %v", err)
	}
	requireOwnerAuthRefusal(t, validateOwnerReplyBinding(fixture.intent, fixture.assumption, fixture.publication, request, reply))
}

func TestOwnerAuthenticationShapePrecedesBootAndRedactsDomainErrors(t *testing.T) {
	fixture := newOwnerAuthFixture(t)
	fixture.intent.Inputs.BootID = otherStableBoot(t, fixture.intent.Inputs.BootID)
	fixture.rebind(t)
	request := fixture.request
	request.RootID = "sensitive-root-value"
	if err := fixture.server.Close(); err != nil {
		t.Fatal(err)
	}
	err := authenticateOwnerRequest(t.Context(), fixture.server, fixture.intent, fixture.assumption, fixture.publication, request)
	if !errors.Is(err, task.ErrInvalidRootID) || errors.Is(err, net.ErrClosed) || errors.Is(err, task.ErrEvidenceFault) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("shape cause lost/rendered or native query ran first: %v", err)
	}
	reply := ownerReply{Protocol: ownerProtocolVersion, RootID: request.RootID, OwnerRef: request.Owner, PublicationSHA256: request.PublicationSHA256, Action: ownerStatus, Code: ownerOK}
	err = validateOwnerReplyBinding(fixture.intent, fixture.assumption, fixture.publication, request, reply)
	if !errors.Is(err, task.ErrInvalidRootID) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("reply binding rendered domain input: %v", err)
	}
}

func TestOwnerAuthenticationDeadlineAndClosedRequestErrorsStayClassifiable(t *testing.T) {
	fixture := newOwnerAuthFixture(t)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	for _, err := range []error{
		authenticateOwnerRequest(ctx, fixture.server, fixture.intent, fixture.assumption, fixture.publication, fixture.request),
		authenticateOwnerPeer(ctx, fixture.client, fixture.intent, fixture.assumption, fixture.publication),
	} {
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("deadline cause lost: %v", err)
		}
	}
	if err := fixture.server.Close(); err != nil {
		t.Fatal(err)
	}
	err := authenticateOwnerRequest(t.Context(), fixture.server, fixture.intent, fixture.assumption, fixture.publication, fixture.request)
	if !errors.Is(err, net.ErrClosed) || errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("closed request peer cause lost: %v", err)
	}
}
