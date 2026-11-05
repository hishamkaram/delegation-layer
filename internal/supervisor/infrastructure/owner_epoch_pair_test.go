package infrastructure

import (
	"context"
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestOwnerEpochPairContainsOnlyValidatedDigests(t *testing.T) {
	typeInfo := reflect.TypeFor[OwnerEpochPair]()
	if typeInfo.NumField() != 2 || typeInfo.Field(0).Name != "ClientSHA256" || typeInfo.Field(1).Name != "DaemonSHA256" {
		t.Fatal("owner pair exposes runtime identity, paths or another selection")
	}
	valid := OwnerEpochPair{ClientSHA256: task.ComputeSHA256([]byte("client")), DaemonSHA256: task.ComputeSHA256([]byte("daemon"))}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []OwnerEpochPair{{}, {ClientSHA256: valid.ClientSHA256}, {DaemonSHA256: valid.DaemonSHA256}, {ClientSHA256: "not-a-digest", DaemonSHA256: valid.DaemonSHA256}} {
		if err := invalid.Validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("invalid pair accepted: %v", err)
		}
	}
}

func serveOwnerEpochStatus(t *testing.T, f ownerContactFixture, code ownerReplyCode) func() error {
	t.Helper()
	return f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
		if request.Action != ownerStatus || request.Owner != f.auth.request.Owner {
			return ErrIdentity
		}
		return writeOwnerFrame(connection, ownerServerReply(request, code))
	})
}

func TestProbeOwnerArtifactsRootOwnerIncludingClosing(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "closing"}[closing], func(t *testing.T) {
			f := newOwnerContactFixture(t)
			if closing {
				body := journalLifecycleBody(t, f.journal, epochLifecycleStop, 1)
				body.Owner = f.auth.request.Owner
				requireLifecycleAccept(t, f.journal, f.lease, body)
				requireLifecycleProgress(t, f.journal, f.lease, body, epochLifecycleClosing)
			}
			before := producerBudgetTree(t, f.auth.root)
			join := serveOwnerEpochStatus(t, f, ownerOK)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			reference, pair, err := ProbeOwnerArtifacts(ctx, f.reader)
			if joinErr := join(); joinErr != nil {
				t.Fatal(joinErr)
			}
			want := OwnerEpochPair{ClientSHA256: f.auth.intent.Inputs.Artifacts.ClientSHA256, DaemonSHA256: f.auth.intent.Inputs.Artifacts.DaemonSHA256}
			if err != nil || reference != f.auth.request.Owner || pair != want {
				t.Fatalf("root-owner observation used a birth gate: pair=%v err=%v", pair, err)
			}
			if !maps.Equal(before, producerBudgetTree(t, f.auth.root)) {
				t.Fatal("owner pair observation changed journal bytes, modes or timestamps")
			}
		})
	}
}

// Canonical journal writers and an actual same-process Unix peer model a
// completed Upgrade. These records prove observation, never native birth.
func ownerEpochUpgradeFixture(t *testing.T) (ownerContactFixture, epochLifecycleIntent) {
	t.Helper()
	auth := newOwnerAuthFixture(t)
	for _, connection := range []*net.UnixConn{auth.client, auth.server} {
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
	}
	journal, err := openIntentJournal(t.Context(), auth.store, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lease, acquired, err := journal.tryAcquire(t.Context())
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil || !acquired {
		t.Fatalf("fixture journal lease: acquired=%t err=%v", acquired, err)
	}
	f := ownerContactFixture{auth: auth, journal: journal, lease: lease, reader: contextPassiveReader(t, journal)}
	body := journalLifecycleBody(t, journal, epochLifecycleOwnerUpgrade, 1)
	body.SuccessorArtifacts = auth.intent.Inputs.Artifacts
	body.SuccessorArtifacts.RunnerSHA256 = task.ComputeSHA256([]byte("future runtime D"))
	body.SuccessorArtifacts.ClientSHA256 = task.ComputeSHA256([]byte("future client D"))
	body.SuccessorArtifacts.DaemonSHA256 = task.ComputeSHA256([]byte("future daemon D"))
	requireLifecycleAccept(t, journal, lease, body)
	requireLifecycleProgress(t, journal, lease, body, epochLifecycleClosing)
	requireLifecycleProgress(t, journal, lease, body, epochLifecycleExiting)
	auth.intent.Inputs.Kind = StartupUpgrade
	auth.intent.Inputs.Artifacts, auth.intent.Inputs.Context = Artifacts{}, LaunchContextReference{}
	auth.intent.Inputs.UpgradeID, auth.intent.Inputs.UpgradeSHA256 = body.ID, lifecycleReference(t, body).SHA256
	auth.publication.RuntimeSHA256 = body.SuccessorArtifacts.RunnerSHA256
	recordOwnerContactFixture(t, f, false)
	progress := lifecycleProgress(t, body, epochLifecycleComplete)
	progress.Successor = f.auth.request.Owner
	if created, putErr := journal.appendLifecycleProgress(t.Context(), lease, lifecycleReference(t, body), progress); putErr != nil || !created {
		t.Fatalf("fixture Upgrade completion: created=%t err=%v", created, putErr)
	}
	return f, body
}

func TestProbeOwnerArtifactsCompletedUpgradeSelectsFuturePair(t *testing.T) {
	f, body := ownerEpochUpgradeFixture(t)
	before := producerBudgetTree(t, f.auth.root)
	join := serveOwnerEpochStatus(t, f, ownerOK)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reference, pair, err := ProbeOwnerArtifacts(ctx, f.reader)
	if joinErr := join(); joinErr != nil {
		t.Fatal(joinErr)
	}
	want := OwnerEpochPair{ClientSHA256: body.SuccessorArtifacts.ClientSHA256, DaemonSHA256: body.SuccessorArtifacts.DaemonSHA256}
	if err != nil || reference != f.auth.request.Owner || pair != want || f.auth.intent.Inputs.Artifacts != (Artifacts{}) {
		t.Fatalf("post-Complete probe lost lifecycle-owned future D: pair=%v err=%v", pair, err)
	}
	if !maps.Equal(before, producerBudgetTree(t, f.auth.root)) {
		t.Fatal("post-Complete observation changed canonical records")
	}
}

func TestProbeOwnerArtifactsReturnsNoPairAlongsideStatusError(t *testing.T) {
	f := newOwnerContactFixture(t)
	join := serveOwnerEpochStatus(t, f, ownerBusy)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reference, pair, err := ProbeOwnerArtifacts(ctx, f.reader)
	if joinErr := join(); joinErr != nil {
		t.Fatal(joinErr)
	}
	if reference != f.auth.request.Owner || !errors.Is(err, ErrOwnerBusy) || pair != (OwnerEpochPair{}) {
		t.Fatalf("reference with probe error became pair selection: reference=%v pair=%v err=%v", reference, pair, err)
	}
}

func TestProbeOwnerArtifactsRejectsMissingOrMalformedLifecycle(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "malformed", true: "missing"}[missing], func(t *testing.T) {
			f, body := ownerEpochUpgradeFixture(t)
			path := filepath.Join(f.auth.root, "infrastructure", lifecycleName(body.ID))
			var err error
			if missing {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte("invalid lifecycle body"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			join := serveOwnerEpochStatus(t, f, ownerOK)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			reference, pair, err := ProbeOwnerArtifacts(ctx, f.reader)
			if joinErr := join(); joinErr != nil {
				t.Fatal(joinErr)
			}
			if err == nil || reference != f.auth.request.Owner || pair != (OwnerEpochPair{}) {
				t.Fatalf("broken referenced lifecycle selected a pair: pair=%v err=%v", pair, err)
			}
		})
	}
}

func TestOwnerStatusRemainsIndependentOfLifecycleSelection(t *testing.T) {
	f, body := ownerEpochUpgradeFixture(t)
	if err := os.Remove(filepath.Join(f.auth.root, "infrastructure", lifecycleName(body.ID))); err != nil {
		t.Fatal(err)
	}
	join := serveOwnerEpochStatus(t, f, ownerOK)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reference, err := ProbeOwner(ctx, f.reader)
	if joinErr := join(); joinErr != nil {
		t.Fatal(joinErr)
	}
	if err != nil || reference != f.auth.request.Owner {
		t.Fatalf("status observation acquired a lifecycle selection dependency: reference=%v err=%v", reference, err)
	}
}

func TestOwnerEpochPairDoesNotRetargetAfterAuthenticatedStatus(t *testing.T) {
	for _, changed := range []string{"startup", "publication", "root"} {
		t.Run(changed, func(t *testing.T) { exerciseOwnerEpochReplacement(t, changed) })
	}
}

func exerciseOwnerEpochReplacement(t *testing.T, changed string) {
	t.Helper()
	f := newOwnerContactFixture(t)
	join := serveOwnerEpochStatus(t, f, ownerOK)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	observed, err := probeOwnerStatus(ctx, f.reader)
	if joinErr := join(); joinErr != nil {
		t.Fatal(joinErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	switch changed {
	case "startup":
		replaceOwnerEpochFixture(t, f)
	case "publication":
		changedPublication := f.auth.publication
		changedPublication.Endpoint.SocketInode++
		writeOwnerEpochPublication(t, f, changedPublication)
	case "root":
		replaceOwnerEpochRoot(t, f.auth.root)
	}
	pair, err := readOwnerEpochPair(ctx, f.reader, observed)
	if err == nil || pair != (OwnerEpochPair{}) {
		t.Fatalf("authenticated owner was silently retargeted: pair=%v err=%v", pair, err)
	}
}

func replaceOwnerEpochFixture(t *testing.T, f ownerContactFixture) {
	t.Helper()
	if err := f.auth.listener.Close(); err != nil {
		t.Fatal(err)
	}
	f.auth.intent.Sequence++
	f.auth.intent.Epoch = task.ComputeSHA256([]byte("replacement owner"))[:32]
	f.auth.intent.Inputs.Artifacts.ClientSHA256 = task.ComputeSHA256([]byte("replacement client"))
	path := ownerEndpointPath(f.auth.root, f.auth.intent.Sequence)
	var err error
	f.auth.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	socket, err := observeEndpointSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	f.auth.publication.Endpoint.Path = path
	f.auth.publication.Endpoint.SocketDev = uint64(socket.Dev)
	f.auth.publication.Endpoint.SocketInode = uint64(socket.Ino)
	recordOwnerContactFixture(t, f, false)
}

func replaceOwnerEpochRoot(t *testing.T, root string) {
	t.Helper()
	moved := root + "-held"
	renamed := false
	t.Cleanup(func() {
		if !renamed {
			return
		}
		if err := os.Remove(root); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
			return
		}
		if err := os.Rename(moved, root); err != nil {
			t.Error(err)
		}
	})
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	renamed = true
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeOwnerEpochPublication(t *testing.T, f ownerContactFixture, publication OwnerPublication) {
	t.Helper()
	data, err := task.MarshalCanonical(publication)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.auth.root, "infrastructure", publicationName(publication.Startup.Sequence)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProbeOwnerArtifactsChecksLifecycleRuntimePublication(t *testing.T) {
	f, _ := ownerEpochUpgradeFixture(t)
	publication := f.auth.publication
	publication.RuntimeSHA256 = task.ComputeSHA256([]byte("foreign runtime E"))
	writeOwnerEpochPublication(t, f, publication)
	join := serveOwnerEpochStatus(t, f, ownerOK)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reference, pair, err := ProbeOwnerArtifacts(ctx, f.reader)
	if joinErr := join(); joinErr != nil {
		t.Fatal(joinErr)
	}
	if !errors.Is(err, ErrIdentity) || reference != f.auth.request.Owner || pair != (OwnerEpochPair{}) {
		t.Fatalf("publication selected a pair from a different runtime: pair=%v err=%v", pair, err)
	}
}

func TestProbeOwnerArtifactsRejectsInvalidContextWithoutContact(t *testing.T) {
	f := newOwnerContactFixture(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, ctx := range []context.Context{nil, context.Background(), canceled} {
		reference, pair, err := ProbeOwnerArtifacts(ctx, f.reader)
		if err == nil || reference != (StartupReference{}) || pair != (OwnerEpochPair{}) {
			t.Fatalf("invalid context selected an owner pair: pair=%v err=%v", pair, err)
		}
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
}
