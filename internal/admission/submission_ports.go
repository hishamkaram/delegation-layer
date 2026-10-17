package admission

import (
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// SubmissionStoreOpener supplies the exact configured record interpreters.
// Admission owns when to open the store and how to validate its inspection;
// application composition retains the provider registry configuration.
type SubmissionStoreOpener func(string) (*taskdir.Store, error)
