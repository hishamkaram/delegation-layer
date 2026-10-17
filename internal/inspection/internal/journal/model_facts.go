package journal

import "github.com/hishamkaram/delegation-layer/internal/inspection/contract"

func (o *Operation) factsLimit() int {
	return contract.FactsLimit(o.request.Binding.DefinitionRevision)
}
