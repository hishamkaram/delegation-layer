package lifetime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Plan is the immutable, durable locator chosen before a native scope is
// created. Registration later records the stronger identity of the created
// object. A plan authorizes inspection of exactly one prospective scope; it
// does not authorize signalling a process.
type Plan struct {
	kind       Kind
	assurance  Assurance
	identifier string
}

// NewPlan reconstructs a durable scope plan after validating its shape.
func NewPlan(kind Kind, assurance Assurance, identifier string) (Plan, error) {
	plan := Plan{kind: kind, assurance: assurance, identifier: identifier}
	if err := validatePlanShape(plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func (p Plan) Kind() Kind           { return p.kind }
func (p Plan) Assurance() Assurance { return p.assurance }
func (p Plan) Identifier() string   { return p.identifier }

// Prepare selects and validates the strongest available backend without
// creating its native resource. ownerKind separates task and inspection
// namespaces for the same task ID.
func Prepare(rootID, ownerKind, taskID string) (Plan, error) {
	if err := validateCreateIDs(rootID, taskID); err != nil {
		return Plan{}, err
	}
	if ownerKind == "" || strings.TrimSpace(ownerKind) == "" || strings.ContainsRune(ownerKind, '\x00') {
		return Plan{}, fmt.Errorf("%w: owner kind is empty or contains NUL", ErrInvalidRef)
	}
	return preparePlatformPlan(rootID, ownerKind, taskID)
}

// CreatePrepared creates exactly the durable plan. It never selects a fallback
// backend after the plan has been persisted.
func CreatePrepared(plan Plan) (*Scope, error) {
	if err := validatePlanShape(plan); err != nil {
		return nil, err
	}
	return createPreparedPlatformScope(plan)
}

// OpenPrepared opens an existing resource at the exact planned location. It
// is used only to reconcile a crash between native creation and registration.
// Process-group plans have no persistent resource before a launch and report
// ErrScopeAbsent.
func OpenPrepared(plan Plan) (*Scope, error) {
	if err := validatePlanShape(plan); err != nil {
		return nil, err
	}
	return openPreparedPlatformScope(plan)
}

// OpenRegistered reopens a registered scope through its original durable
// plan. This allows recovery from a different caller cgroup while still
// proving the exact planned parent, child path, and created object identity.
func OpenRegistered(plan Plan, ref Ref) (*Scope, error) {
	if err := validatePlanShape(plan); err != nil {
		return nil, err
	}
	if err := validateRefShape(ref); err != nil {
		return nil, err
	}
	return openRegisteredPlatformScope(plan, ref)
}

func validatePlanShape(plan Plan) error {
	if plan.identifier == "" || strings.ContainsRune(plan.identifier, '\x00') {
		return fmt.Errorf("%w: empty or NUL scope plan", ErrInvalidRef)
	}
	switch plan.kind {
	case KindCgroupV2:
		if plan.assurance != AssuranceExact {
			return fmt.Errorf("%w: cgroup-v2 plan requires exact assurance", ErrInvalidRef)
		}
		return validatePlatformPlan(plan)
	case KindProcessGroup:
		if plan.assurance != AssuranceCooperative {
			return fmt.Errorf("%w: process-group plan requires cooperative assurance", ErrInvalidRef)
		}
		return validateProcessGroupPlanIdentifier(plan.identifier)
	default:
		return fmt.Errorf("%w: unknown scope plan kind %q", ErrInvalidRef, plan.kind)
	}
}

func deterministicScopeToken(rootID, ownerKind, taskID string) string {
	digest := sha256.Sum256([]byte(rootID + "\x00" + ownerKind + "\x00" + taskID))
	return hex.EncodeToString(digest[:16])
}

func processGroupPlan(rootID, ownerKind, taskID string) Plan {
	return Plan{
		kind:       KindProcessGroup,
		assurance:  AssuranceCooperative,
		identifier: "pg:" + deterministicScopeToken(rootID, ownerKind, taskID),
	}
}
