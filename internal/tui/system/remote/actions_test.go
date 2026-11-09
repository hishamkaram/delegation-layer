package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/app"
	remotecontract "github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remote/payload"
	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

type backendProbe struct{ requests []app.Arguments }

func (p *backendProbe) Remote(_ context.Context, a app.Arguments) app.OperationResult {
	p.requests = append(p.requests, a)
	return app.OperationResult{}
}

func TestRetirementPreviewAndApplyRemainDistinct(t *testing.T) {
	probe := &backendProbe{}
	for _, action := range Actions(probe, app.Arguments{Root: "/state"}) {
		if action.ID != "remote-retire-preview" && action.ID != "remote-retire-apply" {
			continue
		}
		request, err := action.Prepare([]string{"remote:" + strings.Repeat("a", 32), "destination"})
		if err != nil {
			t.Fatal(err)
		}
		apply := action.ID == "remote-retire-apply"
		if action.Confirm != apply || request.RemoteApply != apply || request.RemoteDryRun == apply {
			t.Fatalf("preview/apply confused: %+v", request)
		}
		action.Run(t.Context(), request)
	}
	if len(probe.requests) != 2 {
		t.Fatalf("requests=%+v", probe.requests)
	}
	for _, request := range probe.requests {
		if request.RemoteScope != "destination" || request.Root != "/state" || request.TaskID != "remote:"+strings.Repeat("a", 32) {
			t.Fatalf("selected scope changed: %+v", request)
		}
	}
}

func TestRetirementActionCannotDeleteUploadLeasedOriginBytes(t *testing.T) {
	store, id := originBytes(t)
	backend := app.NewWorkbench(app.Dependencies{Environment: []string{}})
	preview := retirementAction(t, backend, store.Root, "remote-retire-preview")
	request, err := preview.Prepare([]string{"remote:" + id, "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Confirm {
		t.Fatal("preview unexpectedly requires effect confirmation")
	}
	if feedback := preview.Run(t.Context(), request); feedback.Err != nil || !strings.Contains(feedback.Body, `"retirable": true`) {
		t.Fatalf("preview did not inspect real frozen bytes: %+v", feedback)
	}
	lease, err := store.AcquireRemoteByteUse(t.Context(), id, true, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	apply := retirementAction(t, backend, store.Root, "remote-retire-apply")
	request, err = apply.Prepare([]string{"remote:" + id, "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if !apply.Confirm || !request.RemoteApply || request.RemoteScope != "origin" {
		t.Fatal("apply lost exact confirmation/scope")
	}
	feedback := apply.Run(t.Context(), request)
	if !errors.Is(feedback.Err, task.ErrLockBusy) {
		t.Fatalf("retirement bypassed upload lease: %+v", feedback)
	}
	_, snapshot, loadErr := payload.LoadSnapshot(t.Context(), store, id)
	if loadErr != nil {
		t.Fatalf("refused retirement removed owned bytes: %v", loadErr)
	}
	if closeErr := snapshot.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func retirementAction(t *testing.T, backend *app.Workbench, root, id string) ui.Action {
	t.Helper()
	for _, action := range Actions(backend, app.Arguments{Root: root}) {
		if action.ID == id {
			return action
		}
	}
	t.Fatalf("missing retirement action %s", id)
	return ui.Action{}
}

func originBytes(t *testing.T) (*taskdir.Store, string) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	id := strings.Repeat("a", 32)
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "input"), []byte("frozen bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, snapshot, err := payload.PrepareSnapshot(t.Context(), store, id, source, []string{"input"})
	if err != nil {
		t.Fatal(err)
	}
	if err = snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	inputSHA, err := remotefiles.ManifestSHA256(manifest)
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("brief")
	intent := remotecontract.Intent{Version: remotecontract.ProtocolVersion, OriginRootID: store.RootID, TaskID: id, Device: "fixture", Executable: "delegate", Destination: remotecontract.Identity{Host: "fixture", User: "fixture", HostKey: "SHA256:fixture", Port: 22, Root: store.Root, RootID: store.RootID}, Provider: "claude:print", Config: task.TaskConfig{Permission: "read-only", Budget: "1m"}, RemoteCwd: source, Brief: remotecontract.Descriptor{Name: "brief", Length: int64(len(brief)), SHA256: task.ComputeSHA256(brief)}, InputSHA256: inputSHA}
	reference, err := remotecontract.OpenReference(t.Context(), store, id, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reference.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = reference.SaveIntent(t.Context(), intent, brief); err != nil {
		t.Fatal(err)
	}
	digest, err := remotecontract.IntentDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := store.OpenRemoteReferenceContext(t.Context(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	_, putErr := journal.PutContext(t.Context(), "send-intent.json", remotecontract.SendIntent{IntentSHA256: digest})
	if err = errors.Join(putErr, journal.Close()); err != nil {
		t.Fatal(err)
	}
	return store, id
}
