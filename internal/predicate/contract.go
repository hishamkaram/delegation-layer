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
)
