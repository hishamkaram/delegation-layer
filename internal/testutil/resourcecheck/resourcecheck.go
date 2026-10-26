// Package resourcecheck provides a deterministic resource checker for
// fixtures. It never reads the host and is not imported by production
// command binaries.
package resourcecheck

import (
	"context"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
)

// Checker implements the fresh-admission resource port with supplied
// behavior. A nil CheckFunc reports a clear assessment observed now; a nil
// ScopeFunc reports the zero scope.
type Checker struct {
	CheckFunc func(context.Context, resourceadmission.TargetResolver) (resourceadmission.Assessment, error)
	ScopeFunc func(context.Context, resourceadmission.TargetResolver) (resourceadmission.Scope, error)
}

// Clear returns a checker whose every check is clear.
func Clear() *Checker { return &Checker{} }

// Check returns the scripted assessment, or a clear one.
func (c *Checker) Check(ctx context.Context, resolver resourceadmission.TargetResolver) (resourceadmission.Assessment, error) {
	if c.CheckFunc != nil {
		return c.CheckFunc(ctx, resolver)
	}
	return resourceadmission.NewAssessment(time.Now(), resourceadmission.Scope{}, resourceadmission.Findings{}), nil
}

// Scope returns the scripted scope, or the zero scope.
func (c *Checker) Scope(ctx context.Context, resolver resourceadmission.TargetResolver) (resourceadmission.Scope, error) {
	if c.ScopeFunc != nil {
		return c.ScopeFunc(ctx, resolver)
	}
	return resourceadmission.Scope{}, nil
}
