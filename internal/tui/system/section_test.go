package system

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/app"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/host"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

// Any unexpected call into the embedded nil Workbench fails this test: loading
// System is allowed only compiled provider discovery, never native probes.
type backendProbe struct {
	*app.Workbench
	staticCalls int
	devices     []app.Arguments
	preflights  []app.Arguments
}

func (p *backendProbe) Providers() app.ProviderResponse {
	p.staticCalls++
	return app.ProviderResponse{Providers: []commonprovider.Description{{ID: "claude:print", SupportedModes: []string{"read-only"}}}}
}

func (p *backendProbe) Device(_ context.Context, a app.Arguments) app.OperationResult {
	p.devices = append(p.devices, a)
	return app.OperationResult{}
}

func (p *backendProbe) Preflight(_ context.Context, a app.Arguments) app.OperationResult {
	p.preflights = append(p.preflights, a)
	return app.OperationResult{}
}

func TestSystemLoadIsStaticAndControlsAreComplete(t *testing.T) {
	probe := &backendProbe{}
	section := NewSection(probe, app.Arguments{Root: "/state"}, "/project")
	page, err := section.Load(t.Context(), ui.PageRequest{})
	if err != nil || probe.staticCalls != 1 || len(page.Entries) != 2 || len(probe.devices) != 0 {
		t.Fatalf("static load=%+v err=%v", page, err)
	}
	found := make(map[string]bool)
	for _, action := range section.Actions {
		if found[action.ID] {
			t.Fatalf("duplicate control %s", action.ID)
		}
		found[action.ID] = true
	}
	for _, id := range []string{"supervisor-migrate", "supervisor-ensure-owner", "supervisor-stop-owner", "scheduler-stop", "scheduler-migrate", "doctor", "devices-probe", "devices-trust", "workspace-exec", "workspace-forget", "checkpoint-create", "checkpoint-remove", "remote-reconcile", "remote-usage", "remote-retire-preview", "remote-retire-apply"} {
		if !found[id] {
			t.Errorf("missing control %s", id)
		}
	}
	if found["scheduler-serve"] || found["scheduler-run"] {
		t.Fatal("foreground scheduler launcher entered the action inventory")
	}
}

func TestTrustPreservesExactFingerprintAndRequiresConfirmation(t *testing.T) {
	probe := &backendProbe{}
	action := findAction(t, NewSection(probe, app.Arguments{Root: "/state"}, "/project").Actions, "devices-trust")
	a, err := action.Prepare(trustValues(action.Fields))
	if err != nil {
		t.Fatal(err)
	}
	if !action.Confirm || action.Kind != session.Effect {
		t.Fatal("trust was not explicitly confirmed")
	}
	action.Run(t.Context(), a)
	if len(probe.devices) != 1 || probe.devices[0].Fingerprint != "SHA256:exact-user-verified" || probe.devices[0].Device != "review-host" {
		t.Fatalf("trust request changed: %+v", probe.devices)
	}
}

func findAction(t *testing.T, actions []ui.Action, id string) ui.Action {
	t.Helper()
	for _, action := range actions {
		if action.ID == id {
			return action
		}
	}
	t.Fatalf("control %s missing", id)
	return ui.Action{}
}

func trustValues(fields []ui.Field) []string {
	values := make([]string, len(fields))
	for i, f := range fields {
		values[i] = f.Value
		switch f.Label {
		case "SSH alias":
			values[i] = "review-host"
		case "Host fingerprint":
			values[i] = "SHA256:exact-user-verified"
		}
	}
	return values
}

func TestPreflightKeepsExactRequestedProviderAndOptions(t *testing.T) {
	probe := &backendProbe{}
	page, err := NewSection(probe, app.Arguments{Root: "/state"}, "/project").Load(t.Context(), ui.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range page.Entries[1].Actions {
		if action.ID == "preflight" {
			values := preflightValues(action.Fields)
			a, err := action.Prepare(values)
			if err != nil {
				t.Fatal(err)
			}
			if !action.Confirm || action.Kind != session.Effect {
				t.Fatal("preflight was not explicit")
			}
			action.Run(t.Context(), a)
			if len(probe.preflights) != 1 {
				t.Fatal("preflight callback not routed")
			}
			sent := probe.preflights[0]
			if sent.Provider != "claude:print" || sent.Config.Model != "provider-native-model" || sent.Config.Effort != "provider-native-effort" || sent.Config.Permission != "workspace-write" || sent.Cwd != "/target" || sent.Root != "/state" {
				t.Fatalf("request changed: %+v", sent)
			}
			return
		}
	}
	t.Fatal("provider preflight control missing")
}

func preflightValues(fields []ui.Field) []string {
	values := make([]string, len(fields))
	for i, f := range fields {
		values[i] = f.Value
		switch f.Label {
		case "Model":
			values[i] = "provider-native-model"
		case "Effort":
			values[i] = "provider-native-effort"
		case "Permission":
			values[i] = "workspace-write"
		case "Working directory":
			values[i] = "/target"
		}
	}
	return values
}

func TestSchedulerMigrationActionRespectsLiveHostLease(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lease, err := host.AcquireHostLease(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	backend := app.NewWorkbench(app.Dependencies{Environment: []string{}})
	action := findAction(t, NewSection(backend, app.Arguments{Root: store.Root}, t.TempDir()).Actions, "scheduler-migrate")
	if !action.Confirm || action.Kind != session.Effect {
		t.Fatal("migration bypasses confirmation")
	}
	feedback := action.Run(t.Context(), action.Arguments)
	if !errors.Is(feedback.Err, task.ErrLockBusy) {
		t.Fatalf("migration bypassed live host owner: %+v", feedback)
	}
	opened, openErr := scheduler.Open(t.Context(), store)
	if opened != nil {
		t.Cleanup(func() {
			if closeErr := opened.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if !errors.Is(openErr, os.ErrNotExist) {
		t.Fatalf("refused migration initialized storage: %v", openErr)
	}
}
