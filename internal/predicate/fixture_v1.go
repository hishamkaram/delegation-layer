package predicate

import (
	"io"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type fixtureV1 struct{}

func FixtureV1() Interpreter                   { return fixtureV1{} }
func (fixtureV1) Reference() task.PredicateRef { return task.FixturePredicateRef() }
func (fixtureV1) Evaluate(input Input, raw Evidence, out io.Writer) (task.Interpretation, error) {
	decision, err := task.EvaluateRegisteredPredicate(task.FixturePredicateRef(), &input.Seal)
	if err != nil {
		return task.Interpretation{}, err
	}
	result := task.Interpretation{Verdict: decision.Verdict, Refusal: string(decision.PayloadContent)}
	if decision.RawPath != "" {
		err = raw.Read(Stdout, func(r io.Reader) error { _, e := io.Copy(out, r); return e })
	}
	return result, err
}
