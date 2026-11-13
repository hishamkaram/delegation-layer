package taskdir

import (
	"context"
	"path/filepath"
)

// OutcomeReference reads the actual immutable outcome file's digest after the
// normal inspection owner validates its record and publication. It is an
// observation reference, not native-lifetime closure authority.
func (td *TaskDir) OutcomeReference() (string, error) {
	return td.OutcomeReferenceContext(context.Background())
}

func (td *TaskDir) OutcomeReferenceContext(ctx context.Context) (string, error) {
	inspection, err := td.InspectContext(ctx)
	if err != nil || inspection.Outcome == nil {
		return "", err
	}
	digest, _, err := td.store.fileDigestContext(ctx, filepath.Join(td.Dir, "outcome.json"))
	return digest, err
}
