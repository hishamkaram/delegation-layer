package gatewaycommand

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func boundedTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func TestGatewayRootCreatesOnlyNewFeatureEnabledState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	canonical, observation, err := rootContext(boundedTestContext(t), root, predicate.Default(), true)
	if err != nil {
		t.Fatal(err)
	}
	if canonical == "" || observation.RootToken == "" {
		t.Fatal("missing Native root identity")
	}
	for _, feature := range []string{"namespace-admission-v1", "namespace-mutation-lease-v1", "workspace-constrained-connector-v1"} {
		if !slices.Contains(observation.Features, feature) {
			t.Fatalf("missing feature %q: %+v", feature, observation)
		}
	}
	_, again, err := rootContext(boundedTestContext(t), root, predicate.Default(), true)
	if err != nil || again.RootToken != observation.RootToken {
		t.Fatalf("existing enabled state changed: %+v %v", again, err)
	}
}

func TestGatewayRootNeverUpgradesLegacyOrAdoptsPartialState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = rootContext(boundedTestContext(t), root, predicate.Default(), true); !errors.Is(err, ErrOfflineCutoverRequired) {
		t.Fatalf("legacy cutover was automatic: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, "root.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("legacy root changed: %v", err)
	}
	partial := filepath.Join(t.TempDir(), "partial")
	if err = os.Mkdir(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err = rootContext(boundedTestContext(t), partial, predicate.Default(), true); !errors.Is(err, ErrRootExists) {
		t.Fatalf("partial root adopted: %v", err)
	}
	if _, err = os.Stat(filepath.Join(partial, "root.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial root published: %v", err)
	}
}

func TestGatewayRootUsesNormalDefaultResolver(t *testing.T) {
	private := t.TempDir()
	t.Setenv("HOME", private)
	t.Setenv("XDG_CONFIG_HOME", private)
	want, err := config.ResolveDefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := rootContext(boundedTestContext(t), "", predicate.Default(), true)
	if err != nil {
		t.Fatal(err)
	}
	want, err = config.CanonicalizePath(want)
	if err != nil || got != want {
		t.Fatalf("default resolver diverged: got%q want%q err%v", got, want, err)
	}
}

func TestGatewayProfileCannotOccupyNativeTaskNamespace(t *testing.T) {
	root, err := config.CanonicalizePath(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	for _, requested := range []string{root, filepath.Dir(root), filepath.Join(root, "tasks", "profile"), filepath.Join(root, "workspaces")} {
		if _, err = profileDirectory(root, requested); !errors.Is(err, config.ErrRootWorkspaceOverlap) {
			t.Fatalf("profile captured Native namespace %q: %v", requested, err)
		}
	}
	if got, err := profileDirectory(root, ""); err != nil || got != filepath.Join(root, "gateway") {
		t.Fatalf("wrong default profile: %q %v", got, err)
	}
}

func TestGatewayWorkspaceInspectionUsesSavedRootIdentity(t *testing.T) {
	ctx := boundedTestContext(t)
	root, observation, err := rootContext(ctx, filepath.Join(t.TempDir(), "state"), predicate.Default(), true)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	canonical, token, err := inspectWorkspace(ctx, root, observation.RootToken, directory, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := taskdir.ObservePathProjectContext(ctx, canonical)
	if err != nil {
		t.Fatal(err)
	}
	want, err := taskdir.ProjectToken(identity)
	if err != nil || token != want {
		t.Fatalf("workspace path was not bound through Native: %v", err)
	}
	if err = os.Remove(canonical); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(canonical, 0o700); err != nil {
		t.Fatal(err)
	}
	_, replacementToken, err := inspectWorkspace(ctx, root, observation.RootToken, canonical, predicate.Default())
	if err != nil || replacementToken != token {
		t.Fatalf("same-path replacement changed approval: %v", err)
	}
	if _, _, err = inspectWorkspace(ctx, root, "different-root", directory, predicate.Default()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("changed root accepted: %v", err)
	}
	if _, _, err = inspectWorkspace(ctx, root, observation.RootToken, filepath.Join(directory, "absent"), predicate.Default()); err == nil {
		t.Fatal("workspace directory was created or inferred")
	}
}
