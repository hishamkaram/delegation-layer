package taskdir

import "context"

// InspectionOperationIDs lists sorted candidates from the pinned root, including
// partial directories and orphan requests. It neither reads operation records
// nor creates storage. Operation records remain the sole closure authority.
// Missing parents list nothing; malformed, unsafe or symlink entries refuse the
// entire listing rather than hide possible active work.
func (s *Store) InspectionOperationIDs(ctx context.Context) ([]string, error) {
	return s.privateDirectoryIDs(ctx, "inspections", "inspection operation")
}
