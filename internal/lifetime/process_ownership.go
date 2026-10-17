package lifetime

import (
	"fmt"
	"strings"
)

// processScopeOwnershipLedger is the one mutable ownership record for a
// cooperative scope. Registered roots authorize their current process group;
// retained references identify exact discovered processes and do not carry
// that group authority forward.
type processScopeOwnershipLedger struct {
	registered []LaunchRef
	retained   []LaunchRef
}

func (l *processScopeOwnershipLedger) register(ref LaunchRef) {
	l.registered = appendUniqueLaunchRef(l.registered, ref)
}

func (l *processScopeOwnershipLedger) roots(explicit []LaunchRef) []LaunchRef {
	if len(explicit) > 0 {
		roots := make([]LaunchRef, 0, len(explicit))
		for _, ref := range explicit {
			roots = appendUniqueLaunchRef(roots, ref)
		}
		return roots
	}
	return append([]LaunchRef(nil), l.registered...)
}

func (l *processScopeOwnershipLedger) retainedRefs() []LaunchRef {
	return append([]LaunchRef(nil), l.retained...)
}

func (l *processScopeOwnershipLedger) replaceRetained(refs, roots []LaunchRef) {
	retained := make([]LaunchRef, 0, len(refs))
	for _, ref := range refs {
		if containsLaunchRefIdentity(roots, ref) || containsLaunchRefIdentity(retained, ref) {
			continue
		}
		retained = append(retained, ref)
	}
	l.retained = retained
}

func discoverOwnedProcessLaunches(roots []LaunchRef, snapshots []processScopeSnapshot) ([]LaunchRef, error) {
	return discoverOwnedProcessLaunchesWithOwner(roots, nil, nil, nil, snapshots)
}

func discoverOwnedProcessLaunchesWithOwner(
	roots []LaunchRef,
	retained []LaunchRef,
	owner *ProcessRef,
	anchors []ProcessRef,
	snapshots []processScopeSnapshot,
) ([]LaunchRef, error) {
	byPID, err := indexProcessScopeSnapshots(snapshots)
	if err != nil {
		return nil, err
	}
	infrastructure := processInfrastructureRefs(owner, anchors)
	owned, err := seedProcessOwnershipWithRetained(roots, retained, snapshots, byPID, infrastructure)
	if err != nil {
		return nil, err
	}
	ownerLive, err := seedOwnerProcessTree(owner, anchors, snapshots, byPID, owned)
	if err != nil {
		return nil, err
	}
	if owner != nil && !ownerLive {
		return nil, fmt.Errorf("%w: process-scope owner is absent or has changed identity", ErrUnverifiableLaunch)
	}
	if err = expandProcessOwnershipExcluding(owned, snapshots, infrastructure); err != nil {
		return nil, err
	}
	return ownedProcessLaunchRefs(roots, snapshots, owned, infrastructure)
}

func processInfrastructureRefs(owner *ProcessRef, anchors []ProcessRef) []ProcessRef {
	result := make([]ProcessRef, 0, len(anchors)+1)
	if owner != nil {
		result = append(result, *owner)
	}
	for _, anchor := range anchors {
		if !containsProcessRef(result, anchor) {
			result = append(result, anchor)
		}
	}
	return result
}

func containsProcessRef(refs []ProcessRef, candidate ProcessRef) bool {
	for _, ref := range refs {
		if ref.pid == candidate.pid && processIdentityTokensMatch(ref.startToken, candidate.startToken) {
			return true
		}
	}
	return false
}

// processIdentityTokensMatch compares an observed process token with a
// persisted token. Darwin v2 observations carry the stable uniqueid as well
// as the historical start timestamp; v2 matching uses the uniqueid so exec
// cannot invalidate the identity. Existing Darwin records carry only the
// timestamp, so they remain valid only when that checked timestamp matches.
// All other platforms retain exact token comparison.
func processIdentityTokensMatch(observed, persisted string) bool {
	const (
		legacyDarwinPrefix = "darwin:"
		darwinV2Prefix     = "darwin-v2:"
	)
	if observed == persisted {
		if strings.HasPrefix(observed, darwinV2Prefix) {
			_, valid := parseDarwinIdentityToken(observed, darwinV2Prefix, 3)
			return valid
		}
		if strings.HasPrefix(observed, legacyDarwinPrefix) {
			_, valid := parseDarwinIdentityToken(observed, legacyDarwinPrefix, 2)
			return valid
		}
		return true
	}
	if strings.HasPrefix(observed, darwinV2Prefix) && strings.HasPrefix(persisted, darwinV2Prefix) {
		observedParts, observedValid := parseDarwinIdentityToken(observed, darwinV2Prefix, 3)
		persistedParts, persistedValid := parseDarwinIdentityToken(persisted, darwinV2Prefix, 3)
		return observedValid && persistedValid && observedParts[2] == persistedParts[2]
	}
	if strings.HasPrefix(observed, legacyDarwinPrefix) && strings.HasPrefix(persisted, darwinV2Prefix) {
		observed, persisted = persisted, observed
	}
	observedParts, observedValid := parseDarwinIdentityToken(observed, darwinV2Prefix, 3)
	persistedParts, persistedValid := parseDarwinIdentityToken(persisted, legacyDarwinPrefix, 2)
	if !observedValid || !persistedValid {
		return false
	}
	return observedParts[0] == persistedParts[0] && observedParts[1] == persistedParts[1]
}

func parseDarwinIdentityToken(token, prefix string, componentCount int) ([]string, bool) {
	if !strings.HasPrefix(token, prefix) {
		return nil, false
	}
	parts := strings.Split(strings.TrimPrefix(token, prefix), ":")
	if len(parts) != componentCount {
		return nil, false
	}
	for _, part := range parts {
		if !decimalIdentityComponent(part) {
			return nil, false
		}
	}
	return parts, true
}

func decimalIdentityComponent(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func snapshotMatchesProcessRef(snapshot processScopeSnapshot, refs []ProcessRef) bool {
	for _, ref := range refs {
		if snapshot.pid == ref.pid && processIdentityTokensMatch(snapshot.startToken, ref.startToken) {
			return true
		}
	}
	return false
}

func seedOwnerProcessTree(
	owner *ProcessRef,
	anchors []ProcessRef,
	snapshots []processScopeSnapshot,
	byPID map[int]processScopeSnapshot,
	owned *processOwnershipClosure,
) (bool, error) {
	infrastructure := processInfrastructureRefs(owner, anchors)
	if len(infrastructure) == 0 {
		return false, nil
	}
	liveInfrastructure, err := liveInfrastructureProcesses(infrastructure, byPID)
	if err != nil {
		return false, err
	}
	ownerLive := processRefIsLive(owner, liveInfrastructure)
	if len(liveInfrastructure) == 0 {
		return ownerLive, nil
	}
	tree := descendantProcessTree(snapshots, liveInfrastructure)
	if err := addOwnedProcessTree(snapshots, tree, liveInfrastructure, owned); err != nil {
		return false, err
	}
	return ownerLive, nil
}

func liveInfrastructureProcesses(infrastructure []ProcessRef, byPID map[int]processScopeSnapshot) (map[int]processScopeSnapshot, error) {
	live := make(map[int]processScopeSnapshot, len(infrastructure))
	for _, ref := range infrastructure {
		if err := validateProcessRef(ref); err != nil {
			return nil, err
		}
		current, exists := byPID[ref.pid]
		if !exists || !processIdentityTokensMatch(current.startToken, ref.startToken) {
			continue
		}
		live[current.pid] = current
	}
	return live, nil
}

func processRefIsLive(ref *ProcessRef, live map[int]processScopeSnapshot) bool {
	if ref == nil {
		return false
	}
	current, exists := live[ref.pid]
	return exists && processIdentityTokensMatch(current.startToken, ref.startToken)
}

func descendantProcessTree(snapshots []processScopeSnapshot, roots map[int]processScopeSnapshot) map[int]struct{} {
	tree := make(map[int]struct{}, len(roots))
	for pid := range roots {
		tree[pid] = struct{}{}
	}
	for changed := true; changed; {
		changed = false
		for _, snapshot := range snapshots {
			if _, exists := tree[snapshot.pid]; exists {
				continue
			}
			if _, parentOwned := tree[snapshot.parentPID]; !parentOwned {
				continue
			}
			tree[snapshot.pid] = struct{}{}
			changed = true
		}
	}
	return tree
}

func addOwnedProcessTree(
	snapshots []processScopeSnapshot,
	tree map[int]struct{},
	infrastructure map[int]processScopeSnapshot,
	owned *processOwnershipClosure,
) error {
	for _, snapshot := range snapshots {
		if _, inTree := tree[snapshot.pid]; !inTree {
			continue
		}
		if current, infrastructureProcess := infrastructure[snapshot.pid]; infrastructureProcess && processIdentityTokensMatch(current.startToken, snapshot.startToken) {
			continue
		}
		if err := validateOwnedProcessSnapshot(snapshot); err != nil {
			return err
		}
		owned.add(snapshot)
	}
	return nil
}

type processOwnershipClosure struct {
	pids     map[int]struct{}
	groups   map[int]struct{}
	sessions map[int]struct{}
}

func newProcessOwnershipClosure() *processOwnershipClosure {
	return &processOwnershipClosure{
		pids:     make(map[int]struct{}),
		groups:   make(map[int]struct{}),
		sessions: make(map[int]struct{}),
	}
}

func (o *processOwnershipClosure) add(snapshot processScopeSnapshot) {
	o.pids[snapshot.pid] = struct{}{}
	if snapshot.pgid == snapshot.pid {
		o.groups[snapshot.pgid] = struct{}{}
	}
	if snapshot.sessionID == snapshot.pid {
		o.sessions[snapshot.sessionID] = struct{}{}
	}
}

func (o *processOwnershipClosure) addExact(snapshot processScopeSnapshot) {
	o.pids[snapshot.pid] = struct{}{}
}

func (o *processOwnershipClosure) addRoot(snapshot processScopeSnapshot) {
	o.add(snapshot)
	// A durable launch reference binds its current process group to this
	// scope. Other observed processes may extend ownership only when they lead
	// a new group or session; inheriting a shared login session is not proof.
	o.groups[snapshot.pgid] = struct{}{}
}

func (o *processOwnershipClosure) containsPID(pid int) bool {
	_, exists := o.pids[pid]
	return exists
}

func (o *processOwnershipClosure) relates(snapshot processScopeSnapshot) bool {
	_, parentOwned := o.pids[snapshot.parentPID]
	_, groupOwned := o.groups[snapshot.pgid]
	_, sessionOwned := o.sessions[snapshot.sessionID]
	return parentOwned || groupOwned || sessionOwned
}

func indexProcessScopeSnapshots(snapshots []processScopeSnapshot) (map[int]processScopeSnapshot, error) {
	byPID := make(map[int]processScopeSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.pid <= 1 || snapshot.parentPID < 0 {
			return nil, fmt.Errorf(
				"%w: invalid process-scope topology PID=%d PPID=%d",
				ErrUnverifiableLaunch,
				snapshot.pid,
				snapshot.parentPID,
			)
		}
		if _, exists := byPID[snapshot.pid]; exists {
			return nil, fmt.Errorf("%w: duplicate process-scope PID %d", ErrUnverifiableLaunch, snapshot.pid)
		}
		byPID[snapshot.pid] = snapshot
	}
	return byPID, nil
}

func seedProcessOwnershipWithRetained(
	roots []LaunchRef,
	retained []LaunchRef,
	snapshots []processScopeSnapshot,
	byPID map[int]processScopeSnapshot,
	excluded []ProcessRef,
) (*processOwnershipClosure, error) {
	owned := newProcessOwnershipClosure()
	if err := seedProcessOwnershipRoots(owned, roots, byPID); err != nil {
		return nil, err
	}
	if err := seedRetainedProcessOwnership(owned, retained, byPID, excluded); err != nil {
		return nil, err
	}
	if err := seedMarkedProcessOwnership(owned, snapshots, excluded); err != nil {
		return nil, err
	}
	return owned, nil
}

func seedProcessOwnershipRoots(owned *processOwnershipClosure, roots []LaunchRef, byPID map[int]processScopeSnapshot) error {
	for _, root := range roots {
		if err := validateLaunchRefShape(root); err != nil || root.pgid <= 1 {
			return fmt.Errorf("%w: invalid process-scope root", ErrInvalidLaunchRef)
		}
		current, exists := byPID[root.pid]
		if !exists {
			continue
		}
		verified, err := verifyProcessScopeRoot(root, current)
		if err != nil {
			return err
		}
		if verified {
			owned.addRoot(current)
		}
	}
	return nil
}

func seedRetainedProcessOwnership(owned *processOwnershipClosure, retained []LaunchRef, byPID map[int]processScopeSnapshot, excluded []ProcessRef) error {
	for _, ref := range retained {
		if err := validateLaunchRefShape(ref); err != nil || ref.pgid <= 1 {
			return fmt.Errorf("%w: invalid retained process-scope reference", ErrInvalidLaunchRef)
		}
		current, exists := byPID[ref.pid]
		if !exists || !processIdentityTokensMatch(current.startToken, ref.startToken) || snapshotMatchesProcessRef(current, excluded) {
			continue
		}
		if err := validateOwnedProcessSnapshot(current); err != nil {
			return err
		}
		// A retained reference proves only this exact process. It cannot grant
		// ownership to peers that happen to share its group or session.
		owned.addExact(current)
	}
	return nil
}

func seedMarkedProcessOwnership(owned *processOwnershipClosure, snapshots []processScopeSnapshot, excluded []ProcessRef) error {
	for _, snapshot := range snapshots {
		if !snapshot.scopeMarked || owned.containsPID(snapshot.pid) || snapshotMatchesProcessRef(snapshot, excluded) {
			continue
		}
		if err := validateOwnedProcessSnapshot(snapshot); err != nil {
			return err
		}
		owned.add(snapshot)
	}
	return nil
}

func verifyProcessScopeRoot(root LaunchRef, current processScopeSnapshot) (bool, error) {
	if current.startToken == "" {
		return false, fmt.Errorf("%w: PID %d has no stable start identity", ErrUnverifiableLaunch, root.pid)
	}
	if !processIdentityTokensMatch(current.startToken, root.startToken) {
		if current.pgid == root.pgid || current.sessionID == root.pgid {
			return false, fmt.Errorf("%w: PID %d was reused inside recorded process scope %d", ErrUnverifiableLaunch, root.pid, root.pgid)
		}
		return false, nil
	}
	if err := validateOwnedProcessSnapshot(current); err != nil {
		return false, err
	}
	if current.pgid != root.pgid && current.sessionID != root.pgid {
		return false, fmt.Errorf("%w: recorded launch PID %d left process scope %d", ErrUnverifiableLaunch, root.pid, root.pgid)
	}
	return true, nil
}

func expandProcessOwnershipExcluding(owned *processOwnershipClosure, snapshots []processScopeSnapshot, excluded []ProcessRef) error {
	for changed := true; changed; {
		changed = false
		for _, snapshot := range snapshots {
			if owned.containsPID(snapshot.pid) || snapshotMatchesProcessRef(snapshot, excluded) || !owned.relates(snapshot) {
				continue
			}
			if err := validateOwnedProcessSnapshot(snapshot); err != nil {
				return err
			}
			owned.add(snapshot)
			changed = true
		}
	}
	return nil
}

func ownedProcessLaunchRefs(
	roots []LaunchRef,
	snapshots []processScopeSnapshot,
	owned *processOwnershipClosure,
	excluded []ProcessRef,
) ([]LaunchRef, error) {
	result := make([]LaunchRef, 0, len(roots)+len(owned.pids))
	for _, root := range roots {
		result = appendUniqueLaunchRef(result, root)
	}
	for _, snapshot := range snapshots {
		if !owned.containsPID(snapshot.pid) || snapshot.zombie || snapshotMatchesProcessRef(snapshot, excluded) {
			continue
		}
		ref, err := NewLaunchRef(snapshot.pid, snapshot.startToken, snapshot.pgid)
		if err != nil {
			return nil, err
		}
		result = appendUniqueLaunchRef(result, ref)
	}
	return result, nil
}
