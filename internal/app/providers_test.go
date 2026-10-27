package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestProvidersJSONIsBoundedDeterministicAndDoesNotUseTaskFields(t *testing.T) {
	var first, second bytes.Buffer
	if code := Run([]string{"providers", "--json"}, &first, &bytes.Buffer{}, Dependencies{}); code != 0 {
		t.Fatalf("providers failed: code=%d output=%q", code, first.String())
	}
	if code := Run([]string{"providers", "--json"}, &second, &bytes.Buffer{}, Dependencies{}); code != 0 {
		t.Fatalf("second providers call failed: code=%d output=%q", code, second.String())
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("discovery output changed between calls:\n%s\n%s", first.String(), second.String())
	}
	if first.Len() > MaxJSONResponseBytes {
		t.Fatalf("discovery response exceeded bound: %d", first.Len())
	}
	var decoded ProviderResponse
	if err := json.Unmarshal(first.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	assertNativeProviderMetadata(t, decoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(first.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"admission", "liveness", "publication", "task_id", "supervisor"} {
		if _, found := fields[forbidden]; found {
			t.Fatalf("discovery response contains task field %q: %s", forbidden, first.String())
		}
	}
}

func assertNativeProviderMetadata(t *testing.T, response ProviderResponse) {
	t.Helper()
	expectedIDs := []string{"antigravity:print", "ccr:print", "claude:print", "codex:exec", "codex:review", "opencode:run", "pi:json"}
	expectedReadOnly := map[string]commonprovider.ReadOnlyCapability{
		"antigravity:print": {Supported: true, Mechanism: commonprovider.ReadOnlyNativePlan, Containment: commonprovider.ReadOnlyProviderOwned},
		"ccr:print":         {Supported: true, Mechanism: commonprovider.ReadOnlyNativePlan, Containment: commonprovider.ReadOnlyProviderOwned},
		"claude:print":      {Supported: true, Mechanism: commonprovider.ReadOnlyNativePlan, Containment: commonprovider.ReadOnlyProviderOwned},
		"codex:exec":        {Supported: true, Mechanism: commonprovider.ReadOnlyNativeSandbox, Containment: commonprovider.ReadOnlyProviderOwned},
		"codex:review":      {Supported: true, Mechanism: commonprovider.ReadOnlyNativeSandbox, Containment: commonprovider.ReadOnlyProviderOwned},
		"opencode:run":      {Supported: true, Mechanism: commonprovider.ReadOnlyNativePlan, Containment: commonprovider.ReadOnlyProviderOwned},
		"pi:json":           {Supported: true, Mechanism: commonprovider.ReadOnlyToolAllowlist, Containment: commonprovider.ReadOnlyProviderOwned},
	}
	if response.SchemaVersion != OutputSchemaVersion || len(response.Providers) != len(expectedIDs) {
		t.Fatalf("unexpected compiled provider metadata: %+v", response)
	}
	for index, expectedID := range expectedIDs {
		description := response.Providers[index]
		if description.ID != expectedID {
			t.Fatalf("provider at index %d: got %s, want %s", index, description.ID, expectedID)
		}
		if len(description.SupportedModes) == 0 {
			t.Fatalf("supported mode metadata missing: %+v", description)
		}
		// Mirrors the catalog rule: a help command or flag requirements must name
		// an inspectable capability. OpenCode ACP has a help command and no flag.
		if len(description.Runtime.RequiredFlags) == 0 && len(description.Runtime.HelpArgs) == 0 {
			t.Fatalf("runtime capability metadata missing: %+v", description)
		}
		if description.ID != "codex:review" && description.Continuation != commonprovider.ContinuationNative {
			t.Fatalf("continuation metadata missing: %+v", description)
		}
		if description.ID == "codex:review" && description.Continuation != commonprovider.ContinuationUnsupported {
			t.Fatalf("review continuation metadata incorrect: %+v", description)
		}
		if got, want := description.ReadOnly, expectedReadOnly[expectedID]; got != want {
			t.Fatalf("read-only metadata for %s=%+v, want %+v", expectedID, got, want)
		}
	}
}

func TestProvidersWriterFailureIsReturned(t *testing.T) {
	want := errors.New("discovery writer failed")
	if code := runProviders(true, errorWriter{err: want}, &bytes.Buffer{}, NativeCatalog()); code != 1 {
		t.Fatalf("writer failure exit=%d, want 1", code)
	}
}

func TestProvidersRejectsInvalidArgumentsBeforeDiscovery(t *testing.T) {
	for _, args := range [][]string{{"providers", "extra"}, {"providers", "--unknown"}} {
		t.Run(strings.Join(args[1:], "-"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(args, &stdout, &stderr, Dependencies{}); code != 2 {
				t.Fatalf("invalid providers args exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Usage: delegate") {
				t.Fatalf("invalid providers args had wrong streams: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestProvidersDoesNotRequireCurrentProviderEnvironment(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "missing-home"))
	var stdout bytes.Buffer
	if code := Run([]string{"providers", "--json"}, &stdout, &bytes.Buffer{}, Dependencies{}); code != 0 {
		t.Fatalf("discovery depended on current environment: code=%d output=%q", code, stdout.String())
	}
}

func TestDispatchUnsupportedOptionRefusesBeforeSupervisorBinding(t *testing.T) {
	registration, lookupErr := NativeCatalog().Lookup("antigravity:print")
	if lookupErr != nil {
		t.Fatal(lookupErr)
	}
	// Use a deliberately limited catalog so this admission regression remains
	// independent of the native adapter gaining additional options.
	registration.Description.SupportedOptions = []string{commonprovider.OptionContinuation}
	catalog, catalogErr := commonprovider.NewCatalog(registration)
	if catalogErr != nil {
		t.Fatal(catalogErr)
	}

	root := filepath.Join(t.TempDir(), "state")
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("unsupported option"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"dispatch", "--json", "--root", root, "--provider", "antigravity:print", "--brief", brief,
		"--cwd", workspace, "--permission", "workspace-write", "--model", "explicit-model", "--pueue-config", filepath.Join(t.TempDir(), "missing.yml"),
	}, &stdout, &stderr, Dependencies{ResourceChecker: fixtureResources, Catalog: catalog, InitialSupervisorExecutable: filepath.Join(t.TempDir(), "missing-pueue")})
	if code != 2 {
		t.Fatalf("unsupported option exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.TaskRecord != "not_created" || response.Failure == nil ||
		response.Failure.Code != "unsupported_option" ||
		response.Failure.Stage != "prepare-provider" ||
		response.Failure.NextAction != "correct_request" {
		t.Fatalf("unsupported option was not classified safely: %+v", response)
	}
	entries, err := os.ReadDir(filepath.Join(root, "tasks"))
	if err == nil {
		if len(entries) != 0 {
			t.Fatalf("unsupported option created task authority: %v", entries)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestProductionCatalogDoesNotExposeHistoricalFixtureLaunch(t *testing.T) {
	catalog := NativeCatalog()
	if _, err := catalog.Lookup("fixture:test"); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Prepare(taskRecordForProviderTest("fixture:test", "read-only"), os.Environ()); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
		t.Fatalf("fixture acquired production launch authority: %v", err)
	}
}

// TestProductionContinuationRegistrationsDeclareSuccessorPreparation guards
// the new-successor contract of every discoverable production registration
// that can launch and continue: its own PrepareSuccessor decides which
// predecessor facts carry forward, so none may silently fall back to
// reconstructing the predecessor's recorded launch. The generic catalog
// fallback remains available to other compositions.
func TestProductionContinuationRegistrationsDeclareSuccessorPreparation(t *testing.T) {
	catalog := NativeCatalog()
	var checked []string
	for _, description := range catalog.Descriptions() {
		registration, err := catalog.Lookup(description.ID)
		if err != nil {
			t.Fatal(err)
		}
		if registration.Prepare == nil || registration.Description.Continuation == commonprovider.ContinuationUnsupported {
			continue
		}
		checked = append(checked, description.ID)
		if registration.PrepareSuccessor == nil {
			t.Errorf("%s continues without its own successor preparation", description.ID)
		}
	}
	if len(checked) == 0 {
		t.Fatal("production catalog exposes no launchable continuation registration")
	}
	if slices.Contains(checked, "codex:review") || slices.Contains(checked, config.ProviderFixture) {
		t.Fatalf("guard checked a registration without continuation launch: %v", checked)
	}
}

func taskRecordForProviderTest(id, mode string) task.TaskRecord {
	return task.TaskRecord{Provider: id, Mode: mode, RequestedConfig: task.TaskConfig{Permission: mode}}
}
