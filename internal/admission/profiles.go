package admission

// Profiles carries immutable factory and record-reader configuration. Task and
// inspection journals remain the sole owners of durable admission evidence.
type Profiles struct {
	candidates CandidateSource
	openStore  ProfileStoreOpener
}

func NewProfiles(candidates CandidateSource, openStore ProfileStoreOpener) Profiles {
	return Profiles{candidates: candidates, openStore: openStore}
}
