package payload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRemoteDownloadRecoversInterruptedDirectoryReservation(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(fmt.Sprint(populated), func(t *testing.T) {
			parent, journal, intent := interruptedRemoteDownload(t, populated)
			err := journal.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
				directory, openErr := openOwnedDownloadDirectory(parent, parent.Name(), intent, tx)
				if directory != nil {
					openErr = errors.Join(openErr, directory.Close())
				}
				return openErr
			})
			if populated && err == nil {
				t.Fatal("claimed populated uncommitted directory")
			}
			if !populated && err != nil {
				t.Fatalf("empty interrupted reservation unrecoverable: %v", err)
			}
		})
	}
}

func interruptedRemoteDownload(t *testing.T, populated bool) (*os.Root, *taskdir.ControlDir, remote.Intent) {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	intent := remote.Intent{TaskID: strings.Repeat("f", 32)}
	journal, err := store.OpenRemoteReferenceContext(t.Context(), intent.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	parent, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := parent.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	canonical, err := filepath.EvalSymlinks(parent.Name())
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(canonical))
	name := "pending-download-" + hex.EncodeToString(hash[:16]) + ".json"
	if _, err = journal.PutContext(t.Context(), name, remoteDownloadReservation{Path: canonical, TaskID: intent.TaskID}); err != nil {
		t.Fatal(err)
	}
	if err = parent.Mkdir(intent.TaskID, 0o700); err != nil {
		t.Fatal(err)
	}
	if populated {
		if err = parent.WriteFile(filepath.Join(intent.TaskID, "user-data"), []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return parent, journal, intent
}

type remoteFileExchange struct {
	store        *taskdir.Store
	loseChunkAck bool
}

func (e *remoteFileExchange) Call(ctx context.Context, request remote.Request) (remote.Response, error) {
	// Exercise the actual wire preparation; Manifest is intentionally not JSON.
	var wire bytes.Buffer
	writer := remote.Framer{Writer: &wire}
	if err := writer.WriteRequest(request); err != nil {
		return remote.Response{}, err
	}
	reader := remote.Framer{Reader: &wire}
	received, err := reader.ReadRequest()
	if err != nil {
		return remote.Response{}, err
	}
	response := remote.Response{Version: remote.ProtocolVersion, Identity: *received.Expected}
	response, err = Stage(ctx, e.store, received, response)
	if err != nil {
		return remote.Response{}, err
	}
	if received.Operation == remote.StageChunk && e.loseChunkAck {
		e.loseChunkAck = false
		return remote.Response{}, errors.New("nonretryable injected lost acknowledgment")
	}
	return response, nil
}

func TestRemoteSnapshotFrozenBeforeUploadAndLoadsWithoutOriginal(t *testing.T) {
	origin := supervisorfixture.NewInspectionStore(t)
	destination := supervisorfixture.NewInspectionStore(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	_, snapshot, err := PrepareSnapshot(t.Context(), origin, id, source, []string{"file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if err = snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(source, "file.txt")); err != nil {
		t.Fatal(err)
	}
	manifest, snapshot, err := LoadSnapshot(t.Context(), origin, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := snapshot.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	intent := remoteTransferIntent(t, origin, destination, id, manifest)
	coordinator := remote.Coordinator{Exchange: &remoteFileExchange{store: destination}}
	cwd, err := UploadInputs(t.Context(), coordinator, intent, manifest, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyInputs(t.Context(), destination, intent, mustRemoteIntentDigest(t, intent)); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(cwd, "file.txt"))
	if err != nil || string(actual) != "original" {
		t.Fatalf("bytes=%q err=%v", actual, err)
	}
	changed := intent
	changed.InputSHA256 = strings.Repeat("f", 64)
	if err = VerifyInputs(t.Context(), destination, changed, mustRemoteIntentDigest(t, intent)); err == nil {
		t.Fatal("changed input identity admitted")
	}
	changed = intent
	changed.StorageLayout = task.RemoteDataOrganized
	if err = VerifyInputs(t.Context(), destination, changed, mustRemoteIntentDigest(t, intent)); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("different layout verified staged inputs: %v", err)
	}
}

func TestRemoteUploadLostChunkAckResumesSameInput(t *testing.T) {
	origin := supervisorfixture.NewInspectionStore(t)
	destination := supervisorfixture.NewInspectionStore(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("b", 32)
	manifest, snapshot, err := PrepareSnapshot(t.Context(), origin, id, source, []string{"file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := snapshot.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	intent := remoteTransferIntent(t, origin, destination, id, manifest)
	exchange := &remoteFileExchange{store: destination, loseChunkAck: true}
	coordinator := remote.Coordinator{Exchange: exchange}
	if _, err = UploadInputs(t.Context(), coordinator, intent, manifest, snapshot); err == nil {
		t.Fatal("lost acknowledgment not surfaced")
	}
	cwd, err := UploadInputs(t.Context(), coordinator, intent, manifest, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(cwd, "file.txt"))
	if err != nil || string(actual) != "input" {
		t.Fatalf("bytes=%q err=%v", actual, err)
	}
}

func TestRemoteDownloadDirectoryRefusesUnownedOrReplacedPaths(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	id := strings.Repeat("d", 32)
	journal, err := store.OpenRemoteReferenceContext(t.Context(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	parent, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := parent.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	intent := remote.Intent{TaskID: id, Destination: remote.Identity{RootID: strings.Repeat("e", 32)}}
	var owned *os.Root
	err = journal.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		var openErr error
		owned, openErr = openOwnedDownloadDirectory(parent, parent.Name(), intent, tx)
		return openErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = owned.Close(); err != nil {
		t.Fatal(err)
	}
	if err = parent.Rename(id, "retained-original"); err != nil {
		t.Fatal(err)
	}
	if err = parent.Mkdir(id, 0o700); err != nil {
		t.Fatal(err)
	}
	err = journal.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, openErr := openOwnedDownloadDirectory(parent, parent.Name(), intent, tx)
		return openErr
	})
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("replaced download directory accepted: %v", err)
	}
}

func TestRemoteDownloadNeverClaimsExistingUserDirectory(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	id := strings.Repeat("e", 32)
	journal, err := store.OpenRemoteReferenceContext(t.Context(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	parent, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := parent.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err = parent.Mkdir(id, 0o700); err != nil {
		t.Fatal(err)
	}
	intent := remote.Intent{TaskID: id}
	err = journal.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, openErr := openOwnedDownloadDirectory(parent, parent.Name(), intent, tx)
		return openErr
	})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing user directory accepted: %v", err)
	}
}

type remoteDownloadExchange struct{ journal *taskdir.ControlDir }

func (e remoteDownloadExchange) Call(ctx context.Context, request remote.Request) (remote.Response, error) {
	if e.journal != nil {
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		err := e.journal.WithLock(bounded, func(_ *taskdir.ControlTransaction) error { return nil })
		cancel()
		if err != nil {
			return remote.Response{}, fmt.Errorf("network call holds local reference transaction: %w", err)
		}
	}
	response := remote.Response{Version: remote.ProtocolVersion, Identity: *request.Expected}
	selector := request.Transfer.Selector
	if selector == "output:absent" {
		response.Transfer = &remote.TransferReceipt{Availability: "absent", Descriptor: remote.Descriptor{Name: "absent"}}
		return response, nil
	}
	name, content := "result.txt", "answer"
	if selector == "output:present" {
		name, content = "present", "output"
	}
	entry := remotefiles.Entry{Path: name, Size: int64(len(content)), SHA256: task.ComputeSHA256([]byte(content))}
	response.Transfer = &remote.TransferReceipt{Availability: "available", Descriptor: remote.Descriptor{Name: name, Length: entry.Size, SHA256: entry.SHA256}, Offset: request.Transfer.Offset}
	if request.Transfer.Length > 0 {
		end := request.Transfer.Offset + request.Transfer.Length
		response.Data = []byte(content)[request.Transfer.Offset:end]
		response.Transfer.Offset = end
		response.Transfer.Length = int64(len(response.Data))
		response.Transfer.Complete = end == entry.Size
	}
	return response, nil
}

func TestRemoteDownloadReportsAbsentAndContinuesPresentFiles(t *testing.T) {
	destination, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := destination.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	intent := remote.Intent{Version: remote.ProtocolVersion, OriginRootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), Device: "device", Provider: "fixture", RemoteCwd: "/workspace", Destination: remote.Identity{Host: "example", Port: 22, User: "user", HostKey: "SHA256:fixture", Root: "/state", RootID: strings.Repeat("c", 32)}, Brief: remote.Descriptor{Name: "brief", Length: 1, SHA256: task.ComputeSHA256([]byte("x"))}, OutputPaths: []string{"absent", "present"}}
	store := supervisorfixture.NewInspectionStore(t)
	journal, err := store.OpenRemoteReferenceContext(t.Context(), intent.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	reports, err := downloadSelectedFiles(t.Context(), remote.Coordinator{Exchange: remoteDownloadExchange{journal: journal}}, intent, destination, journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 3 || reports[1].Availability != "absent" || reports[1].Download != "not_requested" || reports[2].Download != "complete" {
		t.Fatalf("reports=%+v", reports)
	}
	data, err := destination.ReadFile("files/present")
	if err != nil || string(data) != "output" {
		t.Fatalf("later file=%q err=%v", data, err)
	}
}

func remoteTransferIntent(t *testing.T, origin, destination *taskdir.Store, id string, manifest remotefiles.Manifest) remote.Intent {
	t.Helper()
	inputSHA, err := remotefiles.ManifestSHA256(manifest)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := LayoutInputWorkspace(task.RemoteDataLegacy, destination.Root, destination.RootID, id)
	if err != nil {
		t.Fatal(err)
	}
	return remote.Intent{Version: remote.ProtocolVersion, OriginRootID: origin.RootID, TaskID: id, Device: "worker", Executable: "delegate", Destination: remote.Identity{Host: "example", Port: 22, User: "user", HostKey: "SHA256:fixture", Root: destination.Root, RootID: destination.RootID}, Provider: "claude:print", Config: task.TaskConfig{Permission: "read-only", Budget: "1m"}, RemoteCwd: cwd, Brief: remote.Descriptor{Name: "brief", Length: 5, SHA256: task.ComputeSHA256([]byte("brief"))}, InputSHA256: inputSHA}
}

func mustRemoteIntentDigest(t *testing.T, intent remote.Intent) string {
	t.Helper()
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestRemoteStageIdentityCannotReplaceReadyInput(t *testing.T) {
	origin := supervisorfixture.NewInspectionStore(t)
	destination := supervisorfixture.NewInspectionStore(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "input.txt"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 32)
	manifest, snapshot, err := PrepareSnapshot(t.Context(), origin, id, source, []string{"input.txt"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := snapshot.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	intent := remoteTransferIntent(t, origin, destination, id, manifest)
	coordinator := remote.Coordinator{Exchange: &remoteFileExchange{store: destination}}
	cwd, err := UploadInputs(t.Context(), coordinator, intent, manifest, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	before := supervisorfixture.SnapshotStore(t, destination.Root)
	wrong := remote.Request{Version: 1, TaskID: id, IntentSHA256: strings.Repeat("e", 64), Expected: &intent.Destination, Root: destination.Root, Transfer: &remote.TransferRequest{Selector: "manifest"}, Manifest: &manifest}
	for _, operation := range []remote.Operation{remote.StageBegin, remote.StageChunk, remote.StageCommit} {
		wrong.Operation = operation
		if operation == remote.StageChunk {
			wrong.Transfer = &remote.TransferRequest{Selector: "input:input.txt", Offset: 0, Length: 1}
			wrong.Data = []byte("x")
		}
		if operation == remote.StageCommit {
			wrong.Transfer = &remote.TransferRequest{Selector: "inputs"}
			wrong.Data = nil
		}
		if _, err = Stage(t.Context(), destination, wrong, remote.Response{}); err == nil {
			t.Fatalf("different stage owner accepted for %s", operation)
		}
	}
	if !reflect.DeepEqual(before, supervisorfixture.SnapshotStore(t, destination.Root)) {
		t.Fatal("different intent changed stage journal")
	}
	content, err := os.ReadFile(filepath.Join(cwd, "input.txt"))
	if err != nil || string(content) != "retained" {
		t.Fatalf("ready bytes changed: %q %v", content, err)
	}
	if err = VerifyInputs(t.Context(), destination, intent, strings.Repeat("e", 64)); err == nil {
		t.Fatal("different intent verified staged inputs")
	}
}

func TestRemoteSelfDelegationKeepsOriginSnapshotSeparate(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "input.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("f", 32)
	manifest, snapshot, err := PrepareSnapshot(t.Context(), store, id, source, []string{"input.txt"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := snapshot.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	intent := remoteTransferIntent(t, store, store, id, manifest)
	coordinator := remote.Coordinator{Exchange: &remoteFileExchange{store: store}}
	cwd, err := UploadInputs(t.Context(), coordinator, intent, manifest, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(cwd, "input.txt"), []byte("remote edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	originBytes, err := snapshot.ReadFile("input.txt")
	if err != nil || string(originBytes) != "original" {
		t.Fatalf("execution copy changed retained origin snapshot: %q %v", originBytes, err)
	}
}

func TestRemoteStageMissingOwnerCannotAdoptRetainedBytes(t *testing.T) {
	origin := supervisorfixture.NewInspectionStore(t)
	destination := supervisorfixture.NewInspectionStore(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "input.txt"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("d", 32)
	manifest, snapshot, err := PrepareSnapshot(t.Context(), origin, id, source, []string{"input.txt"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := snapshot.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	intent := remoteTransferIntent(t, origin, destination, id, manifest)
	if _, err = UploadInputs(t.Context(), remote.Coordinator{Exchange: &remoteFileExchange{store: destination}}, intent, manifest, snapshot); err != nil {
		t.Fatal(err)
	}
	ownerPath := filepath.Join(destination.Root, "remote-inputs", id, "stage-owner.json")
	if err = os.Remove(ownerPath); err != nil {
		t.Fatal(err)
	}
	before := supervisorfixture.SnapshotStore(t, destination.Root)
	request := remote.Request{Version: 1, Operation: remote.StageBegin, IntentSHA256: strings.Repeat("e", 64), Root: destination.Root, TaskID: id, Expected: &intent.Destination, Manifest: &manifest, Transfer: &remote.TransferRequest{Selector: "manifest"}}
	if _, err = Stage(t.Context(), destination, request, remote.Response{}); err == nil {
		t.Fatal("retained bytes adopted after owner deletion")
	}
	if !reflect.DeepEqual(before, supervisorfixture.SnapshotStore(t, destination.Root)) {
		t.Fatal("owner deletion replay changed journal")
	}
}
