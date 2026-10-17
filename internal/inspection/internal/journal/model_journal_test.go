package journal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestModelCatalogSurvivesInspectionJournalAndProof(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	binding.DefinitionRevision = provider.ModelsRevision
	op, err := OpenOperation(store, request, binding, time.Unix(100, 0))
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, op.Close()) })
	catalog := provider.ModelCatalog{Status: "available", ReasonCode: "ok", Complete: true, Source: "test"}
	for i := range 100 {
		catalog.Models = append(catalog.Models, provider.ModelInfo{ID: fmt.Sprintf("model-%d", i), Name: strings.Repeat("n", 40)})
	}
	data, err := task.MarshalCanonical(catalog)
	mustInspection(t, err)
	if len(data) <= maxProjectedFacts {
		t.Fatal("fixture does not exceed historical limit")
	}
	permit, err := op.ClaimStart(time.Unix(101, 0))
	mustInspection(t, err)
	mustInspection(t, permit.Consume())
	mustInspection(t, op.RecordReceipt(42))
	mustInspection(t, op.Complete(ResultEligible, data, time.Unix(102, 0)))
	if _, err = op.ReadProof(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing supervisor observation accepted")
	}
	mustInspection(t, op.RecordWorkerSuccess(42))
	reloaded, err := LoadOperation(store, request.TaskID)
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, reloaded.Close()) })
	facts, err := reloaded.ReadProof()
	mustInspection(t, err)
	expected, err := canonicalFactsWithin(data, provider.MaxModelFactsBytes)
	mustInspection(t, err)
	if !bytes.Equal(facts, expected) {
		t.Fatal("model catalog changed across journal recovery")
	}
}
