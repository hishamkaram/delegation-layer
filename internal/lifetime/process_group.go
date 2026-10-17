package lifetime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
)

const processScopeMarkerEnvironment = "DELEGATION_LAYER_PROCESS_SCOPE"

type processGroupScope struct {
	identifier string
	ownership  processScopeOwnershipLedger
	owner      ProcessRef
	ownerBound bool
	anchors    []ProcessRef
	pin        processGroupPin
}

// processGroupPin is compatibility infrastructure for the pre-session Darwin
// references. New scopes use per-process stable identities and do not carry a
// pin through this field.
type processGroupPin interface {
	processRef() ProcessRef
}

// processScopeSnapshot is a point-in-time, identity-bound process observation.
// Cooperative process-group scopes use it to retain descendants across nested
// sessions and process groups created by provider tools. Linux exact cgroups do
// not need this fallback.
type processScopeSnapshot struct {
	pid         int
	parentPID   int
	pgid        int
	sessionID   int
	startToken  string
	scopeMarked bool
	zombie      bool
}

func openProcessGroupScope(ctx context.Context, ref Ref) (*Scope, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if err := validateProcessGroupIdentifier(ref); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identifier, pin, err := openProcessGroupPlatformState(ctx, ref)
	if err != nil {
		return nil, err
	}
	return newScope(ref, &processGroupScope{
		identifier: identifier,
		pin:        pin,
	}), nil
}

func (s *processGroupScope) bindOwnerProcess() error {
	if s == nil {
		return ErrClosed
	}
	if err := configureProcessGroupOwner(); err != nil {
		return err
	}
	ref, err := ObserveProcess(os.Getpid())
	if err != nil {
		return fmt.Errorf("observe process-scope owner: %w", err)
	}
	if s.ownerBound {
		if s.owner.pid == ref.pid && processIdentityTokensMatch(s.owner.startToken, ref.startToken) {
			return nil
		}
		return fmt.Errorf("%w: process-scope owner changed", ErrUnverifiableLaunch)
	}
	s.owner = ref
	s.ownerBound = true
	return bindProcessGroupPlatformState(s)
}

func (s *processGroupScope) trackOwnerProcess(ref ProcessRef) error {
	if s == nil {
		return ErrClosed
	}
	if !s.ownerBound {
		return fmt.Errorf("%w: process-scope owner is not bound", ErrUnverifiableLaunch)
	}
	if err := validateProcessRef(ref); err != nil {
		return err
	}
	for _, existing := range s.anchors {
		if existing.pid == ref.pid && processIdentityTokensMatch(existing.startToken, ref.startToken) {
			return nil
		}
	}
	s.anchors = append(s.anchors, ref)
	return nil
}

func validateProcessGroupIdentifier(ref Ref) error {
	if ref.kind != KindProcessGroup || ref.assurance != AssuranceCooperative {
		return fmt.Errorf("%w: process-group reference has the wrong type", ErrInvalidRef)
	}
	return validateProcessGroupReferenceIdentifier(ref.identifier)
}

func validateProcessGroupPlanIdentifier(identifier string) error {
	if len(identifier) != len("pg:")+32 || identifier[:len("pg:")] != "pg:" {
		return fmt.Errorf("%w: process-group identifier is malformed", ErrInvalidRef)
	}
	for _, char := range identifier[len("pg:"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return fmt.Errorf("%w: process-group identifier is malformed", ErrInvalidRef)
		}
	}
	return nil
}

func (s *processGroupScope) configure(cmd *exec.Cmd) error {
	if err := configureProcessGroupCommand(s, cmd); err != nil {
		return err
	}
	return configureProcessScopeEnvironment(cmd, s.identifier)
}

func (s *processGroupScope) observeStarted(pid int) (LaunchRef, error) {
	present, token, err := observeStartedProcessIdentity(pid)
	if err != nil {
		return LaunchRef{}, fmt.Errorf("observe process-group launch identity: %w", err)
	}
	if !present {
		return LaunchRef{}, fmt.Errorf("%w: process %d is absent", ErrUnverifiableLaunch, pid)
	}
	pgid, err := processGroupID(pid)
	if err != nil {
		return LaunchRef{}, fmt.Errorf("observe process-group membership for PID %d: %w", pid, err)
	}
	if err = validateProcessGroupLaunch(s, pid, pgid); err != nil {
		return LaunchRef{}, err
	}
	if targetErr := validateSignalTarget(pgid); targetErr != nil {
		return LaunchRef{}, targetErr
	}
	launchRef, err := NewLaunchRef(pid, token, pgid)
	if err != nil {
		return LaunchRef{}, err
	}
	s.ownership.register(launchRef)
	return launchRef, nil
}

func (s *processGroupScope) terminate(ctx context.Context, launchRefs []LaunchRef) (resultErr error) {
	if !s.ownerBound {
		defer func() { resultErr = errors.Join(resultErr, processScopeOwnerUnavailable()) }()
	}
	operationCtx, cancel := context.WithTimeout(ctx, processGroupTerminationLimit)
	defer cancel()
	for {
		refs, err := s.refreshLaunchRefs(operationCtx, launchRefs)
		if err != nil {
			return err
		}
		groupRefs, err := groupLaunchRefs(refs)
		if err != nil {
			return err
		}
		if err = validateProcessGroupSignalTargets(s, groupRefs); err != nil {
			return err
		}
		for _, pgid := range sortedGroupIDs(groupRefs) {
			if terminateErr := terminateProcessGroup(operationCtx, s, pgid, groupRefs[pgid]); terminateErr != nil {
				return fmt.Errorf("terminate process group %d: %w", pgid, terminateErr)
			}
		}
		empty, err := s.proveEmptyContext(operationCtx, launchRefs, false)
		if err != nil {
			return err
		}
		if empty {
			return nil
		}
		if err = operationCtx.Err(); err != nil {
			return err
		}
	}
}

func (s *processGroupScope) empty(launchRefs []LaunchRef) (bool, error) {
	return s.emptyContext(context.Background(), launchRefs)
}

func (s *processGroupScope) emptyContext(ctx context.Context, launchRefs []LaunchRef) (bool, error) {
	empty, err := s.proveEmptyContext(ctx, launchRefs, false)
	if err != nil {
		return false, err
	}
	if !s.ownerBound {
		return false, processScopeOwnerUnavailable()
	}
	return empty, nil
}

func (s *processGroupScope) pendingStartEmpty(ctx context.Context) (bool, error) {
	if !s.ownerBound {
		return false, processScopeOwnerUnavailable()
	}
	return s.proveEmptyContext(ctx, nil, true)
}

func (s *processGroupScope) observeEmptyContext(ctx context.Context, launchRefs []LaunchRef, pendingStart bool) (bool, error) {
	if ctx == nil {
		return false, ErrNilContext
	}
	refs, err := s.refreshLaunchRefsWithMode(ctx, launchRefs, pendingStart)
	if err != nil {
		return false, err
	}
	groupRefs, err := groupLaunchRefs(refs)
	if err != nil {
		return false, err
	}
	for _, pgid := range sortedGroupIDs(groupRefs) {
		exists, err := processGroupMembersPresent(pgid, groupRefs[pgid])
		if err != nil {
			return false, fmt.Errorf("observe process group %d: %w", pgid, err)
		}
		if exists {
			return false, nil
		}
	}
	return true, nil
}

func (s *processGroupScope) proveEmptyContext(ctx context.Context, launchRefs []LaunchRef, pendingStart bool) (bool, error) {
	if !s.ownerBound {
		return s.observeEmptyContext(ctx, launchRefs, pendingStart)
	}
	return requireConsecutiveEmptyCensuses(ctx, func(observationCtx context.Context) (bool, error) {
		return s.observeEmptyContext(observationCtx, launchRefs, pendingStart)
	})
}

func processScopeOwnerUnavailable() error {
	return fmt.Errorf("%w: process-scope owner is not bound", ErrUnverifiableLaunch)
}

func requireConsecutiveEmptyCensuses(
	ctx context.Context,
	observe func(context.Context) (bool, error),
) (bool, error) {
	if ctx == nil {
		return false, ErrNilContext
	}
	if observe == nil {
		return false, fmt.Errorf("%w: process-scope census observer is unavailable", ErrUnverifiableLaunch)
	}
	for generation := 0; generation < 2; generation++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		empty, err := observe(ctx)
		if err != nil || !empty {
			return empty, err
		}
	}
	return true, nil
}

func (s *processGroupScope) release() error {
	return releaseProcessGroupPlatformState(s)
}

func (s *processGroupScope) destroyEmpty() error {
	return destroyProcessGroupPlatformState(s)
}

func (s *processGroupScope) reference() Ref {
	return processGroupPlatformReference(s)
}

func (s *processGroupScope) mergeLaunchRefs(launchRefs []LaunchRef) []LaunchRef {
	roots := s.ownership.roots(launchRefs)
	retained := s.ownership.retainedRefs()
	refs := make([]LaunchRef, 0, len(roots)+len(retained))
	for _, ref := range roots {
		refs = appendUniqueLaunchRef(refs, ref)
	}
	for _, ref := range retained {
		refs = appendUniqueLaunchRef(refs, ref)
	}
	return refs
}

func (s *processGroupScope) refreshLaunchRefs(ctx context.Context, launchRefs []LaunchRef) ([]LaunchRef, error) {
	return s.refreshLaunchRefsWithMode(ctx, launchRefs, false)
}

func (s *processGroupScope) refreshLaunchRefsWithMode(ctx context.Context, launchRefs []LaunchRef, pendingStart bool) ([]LaunchRef, error) {
	roots := s.ownership.roots(launchRefs)
	retained := s.ownership.retainedRefs()
	var owner *ProcessRef
	if s.ownerBound {
		ownerRef := s.owner
		owner = &ownerRef
	}
	anchors := append([]ProcessRef(nil), s.anchors...)
	anchors = append(anchors, processGroupPlatformInfrastructure(s)...)
	discovered, err := discoverProcessScopeLaunches(ctx, s.identifier, roots, retained, owner, anchors, pendingStart)
	if err != nil {
		return nil, err
	}
	for _, ref := range discovered {
		if err = validateLaunchRefShape(ref); err != nil || ref.pgid <= 1 {
			return nil, fmt.Errorf("%w: discovered process-scope launch reference is invalid", ErrInvalidLaunchRef)
		}
		if err = validateSignalTarget(ref.pgid); err != nil {
			return nil, err
		}
	}
	for _, root := range roots {
		s.ownership.register(root)
	}
	s.ownership.replaceRetained(discovered, roots)
	return s.mergeLaunchRefs(launchRefs), nil
}

func configureProcessScopeEnvironment(cmd *exec.Cmd, identifier string) error {
	if cmd == nil {
		return fmt.Errorf("%w: nil command", ErrInvalidRef)
	}
	if identifier == "" || strings.ContainsRune(identifier, '\x00') || strings.ContainsRune(identifier, '=') {
		return fmt.Errorf("%w: invalid process-scope marker", ErrInvalidRef)
	}
	prefix := processScopeMarkerEnvironment + "="
	environment := cmd.Env
	if environment == nil {
		environment = os.Environ()
	}
	configured := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		configured = append(configured, entry)
	}
	cmd.Env = append(configured, prefix+identifier)
	return nil
}

func validateOwnedProcessSnapshot(snapshot processScopeSnapshot) error {
	if snapshot.pgid <= 1 || snapshot.sessionID <= 1 || snapshot.startToken == "" {
		return fmt.Errorf(
			"%w: owned process PID=%d has invalid PGID=%d SID=%d or missing start identity",
			ErrUnverifiableLaunch,
			snapshot.pid,
			snapshot.pgid,
			snapshot.sessionID,
		)
	}
	return nil
}

func appendUniqueLaunchRef(refs []LaunchRef, ref LaunchRef) []LaunchRef {
	for _, existing := range refs {
		if existing.pid == ref.pid && existing.pgid == ref.pgid && processIdentityTokensMatch(existing.startToken, ref.startToken) {
			return refs
		}
	}
	return append(refs, ref)
}

func containsLaunchRefIdentity(refs []LaunchRef, candidate LaunchRef) bool {
	for _, ref := range refs {
		if ref.pid == candidate.pid && processIdentityTokensMatch(ref.startToken, candidate.startToken) {
			return true
		}
	}
	return false
}

func groupLaunchRefs(refs []LaunchRef) (map[int][]LaunchRef, error) {
	groups := make(map[int][]LaunchRef)
	for _, ref := range refs {
		if err := validateLaunchRefShape(ref); err != nil || ref.pgid <= 1 {
			return nil, fmt.Errorf("%w: process-group launch reference is invalid", ErrInvalidLaunchRef)
		}
		if err := validateSignalTarget(ref.pgid); err != nil {
			return nil, err
		}
		groups[ref.pgid] = appendUniqueLaunchRef(groups[ref.pgid], ref)
	}
	return groups, nil
}

func sortedGroupIDs(groups map[int][]LaunchRef) []int {
	ids := make([]int, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func terminateProcessGroup(ctx context.Context, scope *processGroupScope, pgid int, refs []LaunchRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exists, err := processGroupMembersPresent(pgid, refs)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	absent, err := sendVerifiedProcessGroupSignal(ctx, scope, pgid, refs, processSignalTerm, "TERM")
	if err != nil || absent {
		return err
	}
	empty, err := waitForProcessGroupGrace(ctx, pgid, refs)
	if err != nil || empty {
		return err
	}
	absent, err = sendVerifiedProcessGroupSignal(ctx, scope, pgid, refs, processSignalKill, "KILL")
	if err != nil || absent {
		return err
	}
	_, err = waitForProcessGroupGrace(ctx, pgid, refs)
	if err == nil {
		return nil
	}
	return fmt.Errorf("wait for process-group absence: %w", err)
}

func sendVerifiedProcessGroupSignal(ctx context.Context, scope *processGroupScope, pgid int, refs []LaunchRef, signal syscall.Signal, name string) (bool, error) {
	err := signalVerifiedProcessGroup(ctx, scope, pgid, refs, signal)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, errProcessNoSuchEntity) {
		return true, requireAbsentProcessGroup(pgid, refs)
	}
	return false, fmt.Errorf("send %s: %w", name, err)
}

func waitForProcessGroupGrace(ctx context.Context, pgid int, refs []LaunchRef) (bool, error) {
	graceCtx, cancel := context.WithTimeout(ctx, processGroupTermGrace)
	err := waitForEmpty(graceCtx, processScopePollInterval, func() (bool, error) {
		return processGroupEmpty(pgid, refs)
	}, sleepContext)
	cancel()
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return false, err
	}
	// The outer scope loop takes a fresh census before another signal pass.
	// This catches children created between the previous census and the exact
	// member signals without allowing one numeric group to consume the entire
	// cleanup deadline.
	return processGroupEmpty(pgid, refs)
}

func verifyLaunchIdentitiesWith(
	pgid int,
	refs []LaunchRef,
	observe func(int) (bool, string, error),
	groupID func(int) (int, error),
) error {
	if len(refs) == 0 {
		return fmt.Errorf("%w: no launch identity for process group %d", ErrUnverifiableLaunch, pgid)
	}
	if observe == nil || groupID == nil {
		return fmt.Errorf("%w: process-group identity observer is unavailable", ErrUnverifiableLaunch)
	}
	var observationErr error
	for _, ref := range refs {
		if ref.pgid != pgid {
			return fmt.Errorf("%w: launch PGID %d does not match target %d", ErrUnverifiableLaunch, ref.pgid, pgid)
		}
		present, token, err := observe(ref.pid)
		if err != nil {
			observationErr = errors.Join(observationErr, fmt.Errorf("inspect PID %d: %w", ref.pid, err))
			continue
		}
		if !present || !processIdentityTokensMatch(token, ref.startToken) {
			continue
		}
		currentPGID, err := groupID(ref.pid)
		if err != nil {
			observationErr = errors.Join(observationErr, fmt.Errorf("inspect PGID for PID %d: %w", ref.pid, err))
			continue
		}
		if currentPGID == pgid {
			return nil
		}
	}
	return errors.Join(
		fmt.Errorf("%w: no recorded launch identity currently owns process group %d", ErrUnverifiableLaunch, pgid),
		observationErr,
	)
}

func processGroupMembersPresentWith(
	pgid int,
	refs []LaunchRef,
	observe func(int) (bool, string, error),
	groupID func(int) (int, error),
) (bool, error) {
	if len(refs) == 0 || observe == nil || groupID == nil {
		return false, fmt.Errorf("%w: process-group identity observer is unavailable", ErrUnverifiableLaunch)
	}
	var observationErr error
	for _, ref := range refs {
		if err := validateLaunchRefShape(ref); err != nil || ref.pgid != pgid {
			return false, fmt.Errorf("%w: launch reference does not belong to process group %d", ErrUnverifiableLaunch, pgid)
		}
		present, token, err := observe(ref.pid)
		if err != nil {
			observationErr = errors.Join(observationErr, fmt.Errorf("inspect PID %d: %w", ref.pid, err))
			continue
		}
		if !present || !processIdentityTokensMatch(token, ref.startToken) {
			continue
		}
		currentPGID, err := groupID(ref.pid)
		if err != nil {
			if errors.Is(err, errProcessNoSuchEntity) {
				continue
			}
			observationErr = errors.Join(observationErr, fmt.Errorf("inspect PGID for PID %d: %w", ref.pid, err))
			continue
		}
		if currentPGID == pgid {
			return true, nil
		}
	}
	return false, observationErr
}

func processGroupEmpty(pgid int, refs []LaunchRef) (bool, error) {
	exists, err := processGroupMembersPresent(pgid, refs)
	return !exists, err
}

func requireAbsentProcessGroup(pgid int, refs []LaunchRef) error {
	empty, err := processGroupEmpty(pgid, refs)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("%w: process group %d remains present", ErrUnverifiableLaunch, pgid)
	}
	return nil
}

func validateSignalTarget(pgid int) error {
	if pgid <= 1 {
		return fmt.Errorf("%w: unsafe process-group ID %d", ErrInvalidLaunchRef, pgid)
	}
	current, err := processGroupID(os.Getpid())
	if err != nil {
		return fmt.Errorf("observe caller process group: %w", err)
	}
	if current == pgid {
		return fmt.Errorf("%w: target is the caller's process group", ErrInvalidLaunchRef)
	}
	return nil
}
