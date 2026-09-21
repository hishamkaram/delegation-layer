package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	expectedIDs := []string{"antigravity:print", "claude:print", "codex:exec", "opencode:run", "pi:json"}
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
		if len(description.Runtime.RequiredFlags) == 0 {
			t.Fatalf("runtime capability metadata missing: %+v", description)
		}
		if description.Continuation != commonprovider.ContinuationNative {
			t.Fatalf("continuation metadata missing: %+v", description)
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
	}, &stdout, &stderr, Dependencies{InitialSupervisorExecutable: filepath.Join(t.TempDir(), "missing-pueue")})
	if code != 2 {
		t.Fatalf("unsupported option exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Error, commonprovider.ErrUnsupportedOption.Error()) {
		t.Fatalf("unsupported option missing from response: %+v", response)
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
	if _, err := catalog.Prepare(taskRecordForProviderTest("fixture:test", "read-only")); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
		t.Fatalf("fixture acquired production launch authority: %v", err)
	}
}

func taskRecordForProviderTest(id, mode string) task.TaskRecord {
	return task.TaskRecord{Provider: id, Mode: mode, RequestedConfig: task.TaskConfig{Permission: mode}}
}
