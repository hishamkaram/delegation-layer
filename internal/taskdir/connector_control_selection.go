package taskdir

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ConnectorTaskSelection guards immutable saved task evidence. It grants no
// admission authority and never resolves or opens the current project path.
// Exactly one project representation is required. An omitted workspace or spec
// adds no constraint; origin and root always match the canonical request.
type ConnectorTaskSelection struct {
	RootToken           string
	ProjectToken        string
	ProjectSHA256       string
	SpecSHA256          string
	Origin              *task.GatewayOrigin
	WorkspaceConstraint *task.WorkspaceConstraint
}

func (s ConnectorTaskSelection) Validate() error {
	if err := task.ValidateSHA256(s.RootToken); err != nil {
		return fmt.Errorf("expected root token: %w", err)
	}
	if err := validateProjectControlSelection(s.ProjectToken, s.ProjectSHA256); err != nil {
		return err
	}
	if s.Origin == nil {
		return errors.New("saved task selection requires an expected origin")
	}
	if err := task.ValidateGatewayOrigin(*s.Origin); err != nil {
		return fmt.Errorf("expected origin: %w", err)
	}
	if s.SpecSHA256 != "" {
		if err := task.ValidateSHA256(s.SpecSHA256); err != nil {
			return fmt.Errorf("expected spec: %w", err)
		}
	}
	return s.WorkspaceConstraint.Validate()
}

// MatchReceipt compares projections of canonical task.json. ProjectSHA256 is
// SHA256 of the exact opaque Native token bytes, not decoded JSON or path bytes.
func (s ConnectorTaskSelection) MatchReceipt(receipt ConnectorReceipt) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if receipt.RootToken != s.RootToken || receipt.Origin != *s.Origin {
		return task.ErrRequestConflict
	}
	if s.ProjectToken != "" && receipt.ProjectToken != s.ProjectToken {
		return task.ErrRequestConflict
	}
	if s.ProjectSHA256 != "" && task.ComputeSHA256([]byte(receipt.ProjectToken)) != s.ProjectSHA256 {
		return task.ErrRequestConflict
	}
	if s.SpecSHA256 != "" && receipt.SpecSHA256 != s.SpecSHA256 {
		return task.ErrRequestConflict
	}
	if expected := s.WorkspaceConstraint; expected != nil && (receipt.WorkspaceConstraint == nil || *expected != *receipt.WorkspaceConstraint) {
		return task.ErrRequestConflict
	}
	return nil
}

func validateProjectControlSelection(token, digest string) error {
	if (token == "") == (digest == "") {
		return errors.New("saved task control requires exactly one expected project token or SHA256")
	}
	if digest != "" {
		if err := task.ValidateSHA256(digest); err != nil {
			return fmt.Errorf("expected project SHA256: %w", err)
		}
		return nil
	}
	_, err := parseProjectToken(token)
	return err
}

// BorrowConnectorControlSelectionContext retains only the root guard, including
// for an absent task. A hash is a comparison guard, never proof of project
// authority: callers must match existing evidence before controlling a task.
func (s *Store) BorrowConnectorControlSelectionContext(ctx context.Context, rootToken, projectToken, projectSHA256 string) (*ConnectorControlGuard, error) {
	if err := validateProjectControlSelection(projectToken, projectSHA256); err != nil {
		return nil, err
	}
	guard := &ConnectorControlGuard{store: s, rootToken: rootToken}
	if err := guard.Validate(ctx); err != nil {
		return nil, err
	}
	return guard, nil
}

func (r *ReadOnlyStore) BorrowConnectorControlSelectionContext(ctx context.Context, rootToken, projectToken, projectSHA256 string) (*ConnectorControlGuard, error) {
	return r.store.BorrowConnectorControlSelectionContext(ctx, rootToken, projectToken, projectSHA256)
}

// BorrowConnectorTaskControlContext matches every selection field against the
// same canonical saved request before borrowing the Store's retained root.
func (s *Store) BorrowConnectorTaskControlContext(ctx context.Context, id string, selection ConnectorTaskSelection) (*ConnectorControlGuard, error) {
	if err := selection.Validate(); err != nil {
		return nil, err
	}
	guard, err := s.BorrowConnectorControlSelectionContext(ctx, selection.RootToken, selection.ProjectToken, selection.ProjectSHA256)
	if err != nil {
		return nil, err
	}
	identity, err := s.RecordedTaskIdentityContext(ctx, id)
	if err != nil {
		return nil, err
	}
	receipt, err := identity.ConnectorReceipt()
	if err != nil {
		return nil, err
	}
	if err = selection.MatchReceipt(*receipt); err != nil {
		return nil, err
	}
	if err = guard.Validate(ctx); err != nil {
		return nil, err
	}
	return guard, nil
}

func (r *ReadOnlyStore) BorrowConnectorTaskControlContext(ctx context.Context, id string, selection ConnectorTaskSelection) (*ConnectorControlGuard, error) {
	return r.store.BorrowConnectorTaskControlContext(ctx, id, selection)
}
