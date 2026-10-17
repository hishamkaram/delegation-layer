package policy

import "slices"

// ChangedSources identifies source changes observed at audit completion.
// Gate and review inputs must still be frozen for the duration of the run.
func ChangedSources(before, after []Source) []string {
	previous := make(map[string]Source, len(before))
	for _, source := range before {
		previous[source.Path] = source
	}
	var changed []string
	for _, source := range after {
		old, present := previous[source.Path]
		if !present || old.SHA256 != source.SHA256 || old.Kind != source.Kind || old.Lines != source.Lines {
			changed = append(changed, source.Path)
		}
		delete(previous, source.Path)
	}
	for path := range previous {
		changed = append(changed, path)
	}
	slices.Sort(changed)
	return changed
}
