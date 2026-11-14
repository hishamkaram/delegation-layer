package admission

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func continuationPathBinding(t *testing.T) *task.ConnectorContext {
	t.Helper()
	return &task.ConnectorContext{
		RootToken:           strings.Repeat("a", 64),
		Project:             task.DirectoryIdentity{Kind: task.PathDirectoryBinding, Path: filepath.Join(t.TempDir(), "project")},
		WorkspaceConstraint: &task.WorkspaceConstraint{Token: "approved-workspace", RelativeProject: "project"},
	}
}

func TestContinuationConnectorContextInheritsDetachedPathBinding(t *testing.T) {
	predecessor := continuationPathBinding(t)
	for _, requested := range []*task.ConnectorContext{nil, predecessor.Clone()} {
		got, err := ContinuationConnectorContext(predecessor, requested)
		if err != nil || !reflect.DeepEqual(got, predecessor) {
			t.Fatalf("binding = %+v, %v; want predecessor path binding", got, err)
		}
		got.WorkspaceConstraint.RelativeProject = "changed"
		if predecessor.WorkspaceConstraint.RelativeProject != "project" || requested != nil && requested.WorkspaceConstraint.RelativeProject != "project" {
			t.Fatal("successor shares a mutable workspace constraint")
		}
	}
}

func TestValidateContinuationRejectsPathBindingChangesBeforeProviderPreparation(t *testing.T) {
	predecessor := task.TaskRecord{ConnectorContext: continuationPathBinding(t)}
	for _, change := range []struct {
		name string
		edit func(*task.TaskRecord)
	}{
		{"stripped", func(r *task.TaskRecord) { r.ConnectorContext = nil }},
		{"root", func(r *task.TaskRecord) { r.ConnectorContext.RootToken = strings.Repeat("b", 64) }},
		{"path", func(r *task.TaskRecord) {
			r.ConnectorContext.Project.Path = filepath.Dir(r.ConnectorContext.Project.Path)
		}},
		{"physical kind", func(r *task.TaskRecord) { r.ConnectorContext.Project.Kind, r.ConnectorContext.Project.Inode = "", 1 }},
		{"workspace stripped", func(r *task.TaskRecord) { r.ConnectorContext.WorkspaceConstraint = nil }},
		{"workspace token", func(r *task.TaskRecord) { r.ConnectorContext.WorkspaceConstraint.Token = "other" }},
		{"workspace selection", func(r *task.TaskRecord) { r.ConnectorContext.WorkspaceConstraint.RelativeProject = "other" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			successor := task.TaskRecord{CanonicalCwd: predecessor.ConnectorContext.Project.Path, ConnectorContext: predecessor.ConnectorContext.Clone()}
			change.edit(&successor)
			// A zero catalog/profiles would fail differently if binding validation
			// let this request reach provider preparation.
			if _, err := ValidateContinuation(StaticOptions{}, "", predecessor, task.MetaRecord{}, &task.PriorSession{}, successor); !errors.Is(err, task.ErrRequestConflict) {
				t.Fatalf("binding change reached later admission: %v", err)
			}
		})
	}
}

func TestContinuationConnectorContextLeavesLegacyPredecessorsUnbound(t *testing.T) {
	for _, predecessor := range []*task.ConnectorContext{nil, {Project: task.DirectoryIdentity{Path: filepath.Join(t.TempDir(), "legacy"), Inode: 1}}} {
		got, err := ContinuationConnectorContext(predecessor, nil)
		if got != nil || err != nil {
			t.Fatalf("legacy continuation gained binding: %+v, %v", got, err)
		}
	}
}

func TestValidateContinuationRejectsCwdOutsideSavedPathBinding(t *testing.T) {
	predecessor := task.TaskRecord{ConnectorContext: continuationPathBinding(t)}
	successor := task.TaskRecord{CanonicalCwd: filepath.Dir(predecessor.ConnectorContext.Project.Path), ConnectorContext: predecessor.ConnectorContext.Clone()}
	if _, err := ValidateContinuation(StaticOptions{}, "", predecessor, task.MetaRecord{}, &task.PriorSession{}, successor); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("successor cwd escaped the saved binding before static admission: %v", err)
	}
}
