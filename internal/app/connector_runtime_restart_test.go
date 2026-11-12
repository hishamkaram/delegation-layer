package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (f *connectorFixture) seedRuntimeRestartFence(t *testing.T) {
	t.Helper()
	_, meta, err := f.task.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	binding := meta.SupervisorConfig
	binding.ConfigPath = pueue.PrivateConfigPath(f.store.Root)
	from := pueue.RuntimeRestartReference(fmt.Sprintf("v1:%s:1:%s:%s", f.store.RootID, strings.Repeat("1", 32), strings.Repeat("a", 64)))
	writeRuntimeRestartFenceFixture(t, t.Context(), f.store.Root, binding, from)
}

func writeRuntimeRestartFenceFixture(t *testing.T, ctx context.Context, root string, binding task.SupervisorRef, from pueue.RuntimeRestartReference) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(binding.ConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	type restartRecord struct {
		From            pueue.RuntimeRestartReference `json:"from"`
		Binding         task.SupervisorRef            `json:"binding"`
		Phase           pueue.RuntimeRestartPhase     `json:"phase"`
		OldPID          int                           `json:"old_pid"`
		OldProcessStart string                        `json:"old_process_start"`
	}
	// Journal data only; the recorded identity has no native daemon behind it.
	data, err := task.MarshalCanonical(struct {
		SchemaVersion int           `json:"schema_version"`
		Restart       restartRecord `json:"runtime_restart"`
	}{SchemaVersion: task.SchemaVersion, Restart: restartRecord{
		From:    from,
		Binding: binding, Phase: pueue.RuntimeRestartRequested, OldPID: 101, OldProcessStart: "recorded-only",
	}})
	if err != nil {
		t.Fatal(err)
	}
	writeAppTestFile(t, filepath.Join(filepath.Dir(binding.ConfigPath), "bootstrap.pending"), data)
	if pending, readErr := pueue.PrivateRuntimeRestartPending(ctx, root); readErr != nil || !pending {
		t.Fatalf("invalid runtime fence fixture: pending=%t err=%v", pending, readErr)
	}
}

func TestConnectorContextRuntimeRestartScrubsReadiness(t *testing.T) {
	f := newConnectorFixture(t)
	f.seedRuntimeRestartFence(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"context", "inspect", "--root", f.store.Root, "--cwd", f.request.CanonicalCwd, "--json"}, &stdout, &stderr, f.deps)
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("context response exit=%d stderr=%q: %v", code, stderr.String(), err)
	}
	if code != 3 || response.Connector == nil {
		t.Fatalf("pending restart advertised a ready context: code=%d response=%+v", code, response)
	}
	if response.Connector.RootToken != "" || response.Connector.ProjectToken != "" || len(response.Connector.Features) != 0 || response.Connector.OriginBarrier != nil {
		t.Fatalf("pending restart retained readiness proof: %+v", response.Connector)
	}
}

func TestConnectorCommittedResultRemainsReadableDuringRuntimeRestart(t *testing.T) {
	f := newConnectorFixture(t)
	const accepted = "accepted fixture answer"
	outcome := commitConnectorFixtureResult(t, f, accepted)
	f.seedRuntimeRestartFence(t)
	originFile := filepath.Join(t.TempDir(), "origin.json")
	origin, err := task.MarshalCanonical(f.origin)
	if err != nil {
		t.Fatal(err)
	}
	writeAppTestFile(t, originFile, origin)
	descriptor, err := task.MarshalCanonical(outcome.Payload)
	if err != nil {
		t.Fatal(err)
	}
	deps := f.deps
	deps.Input = bytes.NewReader(descriptor)
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"result", "read", f.request.TaskID, "--root", f.store.Root,
		"--expected-root", f.rootToken, "--expected-project", f.projectToken,
		"--expected-origin", originFile, "--expected-descriptor", "-",
		"--offset", "0", "--limit", "1024", "--json",
	}, &stdout, &stderr, deps)
	var response Response
	if err = json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("result response exit=%d stderr=%q: %v", code, stderr.String(), err)
	}
	if code != 0 || response.Connector == nil || response.Connector.ResultRange == nil {
		t.Fatalf("runtime fence hid accepted result: code=%d response=%+v", code, response)
	}
	if got := string(response.Connector.ResultRange.Data); got != accepted {
		t.Fatalf("runtime fence changed accepted bytes: %q", got)
	}
}
