package taskdir

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func savedConnectorSelection(t *testing.T, store *Store, id string) (ConnectorTaskSelection, ConnectorReceipt) {
	t.Helper()
	identity, err := store.RecordedTaskIdentityContext(t.Context(), id)
	must(t, err)
	receipt, err := identity.ConnectorReceipt()
	must(t, err)
	digest := sha256.Sum256([]byte(receipt.ProjectToken))
	return ConnectorTaskSelection{
		RootToken: receipt.RootToken, ProjectSHA256: fmt.Sprintf("%x", digest),
		Origin: receipt.Origin.Clone(), SpecSHA256: receipt.SpecSHA256,
		WorkspaceConstraint: receipt.WorkspaceConstraint.Clone(),
	}, *receipt
}

func TestConnectorTaskSelectionSurvivesRemovedProject(t *testing.T) {
	store, td, _, workspace := workspacePreparedTask(t)
	selection, receipt := savedConnectorSelection(t, store, td.TaskID)
	before, err := os.ReadFile(filepath.Join(td.Dir, "task.json"))
	must(t, err)
	must(t, os.RemoveAll(workspace))
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	guard, err := reader.BorrowConnectorTaskControlContext(t.Context(), td.TaskID, selection)
	must(t, err)
	must(t, guard.Validate(t.Context()))
	observed, err := reader.ReadConnectorReceiptContext(t.Context(), td.TaskID)
	must(t, err)
	if !reflect.DeepEqual(receipt, *observed) {
		t.Fatal("removed project changed canonical receipt")
	}
	legacy := selection
	legacy.ProjectSHA256, legacy.ProjectToken = "", receipt.ProjectToken
	guard, err = store.BorrowConnectorTaskControlContext(t.Context(), td.TaskID, legacy)
	must(t, err)
	must(t, guard.Validate(t.Context()))
	after, err := os.ReadFile(filepath.Join(td.Dir, "task.json"))
	must(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("historical guard rewrote saved task request")
	}
	if _, err = store.RequireConnectorContext(t.Context(), selection.RootToken, selection.ProjectSHA256, workspace); err == nil {
		t.Fatal("historical hash became new admission authority")
	}
}

func TestConnectorTaskSelectionRejectsIncorrectCanonicalGuardsWithoutMutation(t *testing.T) {
	store, td, _, workspace := workspacePreparedTask(t)
	selection, receipt := savedConnectorSelection(t, store, td.TaskID)
	must(t, os.RemoveAll(workspace))
	decoded, err := base64.RawURLEncoding.DecodeString(receipt.ProjectToken)
	must(t, err)
	changes := []struct {
		name string
		edit func(*ConnectorTaskSelection)
	}{
		{"root", func(s *ConnectorTaskSelection) { s.RootToken = strings.Repeat("0", 64) }},
		{"project", func(s *ConnectorTaskSelection) { s.ProjectSHA256 = strings.Repeat("0", 64) }},
		{"decoded token hash", func(s *ConnectorTaskSelection) { s.ProjectSHA256 = fmt.Sprintf("%x", sha256.Sum256(decoded)) }},
		{"origin", func(s *ConnectorTaskSelection) { s.Origin.OperationID = "other-operation" }},
		{"spec", func(s *ConnectorTaskSelection) { s.SpecSHA256 = strings.Repeat("0", 64) }},
		{"workspace", func(s *ConnectorTaskSelection) { s.WorkspaceConstraint.RelativeProject = "other-project" }},
	}
	before := snapshotTree(t, store.Root)
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			candidate := selection
			candidate.Origin = selection.Origin.Clone()
			candidate.WorkspaceConstraint = selection.WorkspaceConstraint.Clone()
			change.edit(&candidate)
			_, err := store.BorrowConnectorTaskControlContext(t.Context(), td.TaskID, candidate)
			if !errors.Is(err, task.ErrRequestConflict) && !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("incorrect %s guard: %v", change.name, err)
			}
			if !reflect.DeepEqual(before, snapshotTree(t, store.Root)) {
				t.Fatal("rejected selection mutated task store")
			}
		})
	}
}

func TestConnectorTaskSelectionRejectsInvalidBoundaryValues(t *testing.T) {
	store, td, _, _ := workspacePreparedTask(t)
	selection, receipt := savedConnectorSelection(t, store, td.TaskID)
	changes := []struct {
		name string
		edit func(*ConnectorTaskSelection)
	}{
		{"no project", func(s *ConnectorTaskSelection) { s.ProjectSHA256 = "" }},
		{"both project forms", func(s *ConnectorTaskSelection) { s.ProjectToken = receipt.ProjectToken }},
		{"uppercase hash", func(s *ConnectorTaskSelection) { s.ProjectSHA256 = strings.Repeat("A", 64) }},
		{"invalid token", func(s *ConnectorTaskSelection) { s.ProjectSHA256, s.ProjectToken = "", "invalid" }},
		{"missing origin", func(s *ConnectorTaskSelection) { s.Origin = nil }},
		{"invalid origin", func(s *ConnectorTaskSelection) { s.Origin.OperationID = "invalid operation" }},
		{"invalid root", func(s *ConnectorTaskSelection) { s.RootToken = "invalid" }},
		{"invalid spec", func(s *ConnectorTaskSelection) { s.SpecSHA256 = "invalid" }},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			candidate := selection
			candidate.Origin = selection.Origin.Clone()
			change.edit(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid selection passed boundary validation")
			}
			if err := candidate.MatchReceipt(receipt); err == nil {
				t.Fatal("invalid selection matched saved receipt")
			}
		})
	}
}

func TestConnectorHashControlGuardPreservesRootCustodyForAbsentTask(t *testing.T) {
	store, td, _, workspace := workspacePreparedTask(t)
	selection, _ := savedConnectorSelection(t, store, td.TaskID)
	must(t, os.RemoveAll(workspace))
	guard, err := store.BorrowConnectorControlSelectionContext(t.Context(), selection.RootToken, "", selection.ProjectSHA256)
	must(t, err)
	must(t, guard.Validate(t.Context()))
	_, err = store.BorrowConnectorTaskControlContext(t.Context(), strings.Repeat("f", 32), selection)
	if !task.RecordAbsent(err) {
		t.Fatalf("missing canonical request was not absent: %v", err)
	}
	replacePathDirectory(t, store.Root)
	if err = guard.Validate(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("hash control guard ignored root replacement: %v", err)
	}
}
