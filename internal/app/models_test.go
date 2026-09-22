package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestModelsArguments(t *testing.T) {
	for _, args := range [][]string{
		{"models", "--provider", "codex:exec", "--json"},
		{"models", "--provider", "antigravity:print", "--cwd", "/workspace"},
	} {
		parsed, err := ParseArguments(args)
		if err != nil || parsed.Command != "models" {
			t.Fatalf("parse %v: %+v %v", args, parsed, err)
		}
	}
	for _, args := range [][]string{
		{"models"},
		{"models", "--provider", "codex:exec", "--cwd", "relative"},
		{"models", "--provider", "codex:exec", "--effort", "high"},
	} {
		if _, err := ParseArguments(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestModelsUnavailableProviderHasJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"models", "--provider", "missing:provider", "--json"}, &stdout, &stderr, Dependencies{})
	var response ModelsResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("non-JSON response: %s (%v)", stdout.String(), err)
	}
	if code != 2 || response.Status != "unavailable" || response.Complete || len(response.Models) != 0 || stderr.Len() != 0 {
		t.Fatalf("incorrect result: %d %+v stderr=%s", code, response, stderr.String())
	}
}

func TestModelsInvalidDirectoriesUseInvalidRequestExitCode(t *testing.T) {
	root := t.TempDir()
	workspaceFile := filepath.Join(t.TempDir(), "workspace-file")
	if err := os.WriteFile(workspaceFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{workspaceFile, root} {
		t.Run(filepath.Base(cwd), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run([]string{"--root", root, "models", "--provider", "missing:provider", "--cwd", cwd, "--json"}, &stdout, &stderr, Dependencies{})
			var response ModelsResponse
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatalf("non-JSON response: %s (%v)", stdout.String(), err)
			}
			if code != 2 || response.Status != "failed" || stderr.Len() != 0 {
				t.Fatalf("invalid request result: code=%d response=%+v stderr=%s", code, response, stderr.String())
			}
		})
	}
}

func TestDiscoveryCandidateDoesNotRequireDispatchFlagsOrFinalize(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	interpreter, err := predicate.Default().Resolve(task.FixturePredicateRef())
	if err != nil {
		t.Fatal(err)
	}
	original := commonprovider.InspectionDefinition{
		Revision: commonprovider.RuntimeInspectionRevision, Executable: "/fake/provider", ExecutableSHA256: task.ComputeSHA256([]byte("fake")),
		Directory: workspace, Environment: []string{}, OutputLimit: commonprovider.MaxInspectionOutput,
		Runtime: &commonprovider.RuntimeProbeDefinition{Executable: "/fake/provider", ExecutableSHA256: task.ComputeSHA256([]byte("fake")), Directory: workspace, Environment: []string{}, RequiredFlags: []string{"--dispatch-only"}},
	}
	registration := commonprovider.Registration{
		Description:  commonprovider.Description{ID: config.ProviderFixture, SupportedModes: []string{config.ModeReadOnly}, Discoverable: true, Runtime: commonprovider.RuntimeCapability{RequiredFlags: []string{"--dispatch-only"}}},
		Interpreters: []predicate.Interpreter{interpreter},
		Prepare: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
			return commonprovider.ProfileCandidate{Directory: workspace, Inspection: &original, Finalize: func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
				t.Fatal("discovery finalized a task")
				return commonprovider.PreparedProfile{}, nil
			}}, nil
		},
		Models: func() commonprovider.ModelDiscoveryDefinition {
			return commonprovider.ModelDiscoveryDefinition{Arguments: []string{"models"}, Source: "fixture", Project: func([]byte, []byte, []byte, bool) (commonprovider.ModelCatalog, error) {
				t.Fatal("static discovery launched projection")
				return commonprovider.ModelCatalog{}, nil
			}}
		},
	}
	catalog, err := commonprovider.NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := prepareModelsCandidate(Dependencies{Catalog: catalog}, root, task.TaskRecord{Provider: config.ProviderFixture, CanonicalCwd: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Inspection.Runtime != nil || candidate.Inspection.Models == nil || original.Runtime == nil || original.Models != nil {
		t.Fatal("discovery leaked dispatch checks or mutated its definition")
	}
}
