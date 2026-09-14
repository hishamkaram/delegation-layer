// Package predicate interprets sealed evidence without launch or storage authority.
package predicate

import (
	"io"

	"github.com/hishamkaram/delegation-layer/internal/task"
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
	Interpreter interface {
		Reference() task.PredicateRef
		Evaluate(Input, Evidence, io.Writer) (task.Interpretation, error)
	}
)
