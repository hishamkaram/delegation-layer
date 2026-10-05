// Package predicate interprets sealed evidence without launch or storage authority.
package predicate

import (
	"errors"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ErrEvidenceAbsent means a declared optional artifact was absent at sealing.
// Undeclared names and missing/corrupt sealed files are evidence faults instead.
var ErrEvidenceAbsent = errors.New("declared optional evidence is absent")

// Recovery selection is deliberately separate from ordinary predicate
// resolution. A recovery target is an explicitly declared, current
// interpreter; it is never synthesized by changing the source predicate
// reference.
var (
	ErrRecoveryUnsupported = errors.New("predicate recovery is unsupported")
	ErrRecoveryAmbiguous   = errors.New("predicate recovery target is ambiguous")
)

type Stream uint8

const (
	Stdout Stream = iota + 1
	Stderr
)

type (
	Input struct {
		Seal            task.ProviderExitRecord
		ExpectedSession task.SessionExpectation
		RecordedSession *task.SessionIdentity
	}
	// Evidence supplies callback-scoped readers; retaining a reader does not retain access.
	Evidence interface {
		Read(Stream, func(io.Reader) error) error
	}
	// NamedEvidence extends streams without changing historical interpreter APIs.
	NamedEvidence interface {
		ReadNamed(string, func(io.Reader) error) error
	}
	Interpreter interface {
		Reference() task.PredicateRef
		Evaluate(Input, Evidence, io.Writer) (task.Interpretation, error)
	}
	// RecoveryInterpreter is the narrow consumer-owned contract used by
	// offline rechecks. Reference identifies the registered interpreter;
	// Target identifies the explicit derived assessment predicate. Recover is
	// passed the original sealed input and must not rewrite its seal predicate.
	RecoveryInterpreter interface {
		Target() task.PredicateRef
		Reference() task.PredicateRef
		CanRecover(source task.PredicateRef) bool
		Recover(Input, Evidence, io.Writer) (task.Interpretation, error)
	}
)
