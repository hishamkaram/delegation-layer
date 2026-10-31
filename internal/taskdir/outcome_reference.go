package taskdir

import "path/filepath"

// OutcomeReference reads the actual immutable outcome file's digest after the
// normal inspection owner validates its record and publication. It is an
// observation reference, not native-lifetime closure authority.
func (td *TaskDir) OutcomeReference() (string, error) {
	inspection, err := td.Inspect()
	if err != nil || inspection.Outcome == nil {
		return "", err
	}
	digest, _, err := td.store.fileDigest(filepath.Join(td.Dir, "outcome.json"))
	return digest, err
}
