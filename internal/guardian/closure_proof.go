package guardian

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ErrProofPending means the observed records do not yet prove lifetime closure.
var ErrProofPending = errors.New("lifetime closure proof is pending")

// ClosureProof is an immutable projection of validated lifetime records.
// The journal owns the records; task.ResolveLifetimeClosure owns their meaning.
type ClosureProof struct {
	closureKind       task.LifetimeClosureKind
	terminationSHA256 string
	workerWaitSHA256  string
	unlaunchedSHA256  string
}

func (p ClosureProof) Kind() task.LifetimeClosureKind { return p.closureKind }

func (p ClosureProof) TerminationSHA256() string { return p.terminationSHA256 }

func (p ClosureProof) WorkerWaitSHA256() string { return p.workerWaitSHA256 }

func (p ClosureProof) UnlaunchedSHA256() string { return p.unlaunchedSHA256 }

// Closed accepts the canonical resolver's dispositions or an unlaunched fence.
// Requiring every native hash would reject valid recovery dispositions.
func (p ClosureProof) Closed() bool {
	return (task.LifetimeClosure{Kind: p.closureKind}).Closed() || p.unlaunchedSHA256 != ""
}
