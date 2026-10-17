package contract

import (
	"testing"
	"time"

	provider "github.com/hishamkaram/delegation-layer/internal/provider/contract"
)

func TestInspectionDeadlinesPreserveHistoricalJournals(t *testing.T) {
	created := time.Unix(100, 0).UTC()
	for _, revision := range []string{provider.ModelsRevision, provider.RuntimeInspectionRevision, "legacy"} {
		binding := Binding{DefinitionRevision: revision}
		want := AdmissionTimeout
		if revision == provider.ModelsRevision {
			want = ModelDiscoveryTimeout
		}
		if got := TimeoutForRevision(revision); got != want {
			t.Fatalf("revision %q timeout=%s want=%s", revision, got, want)
		}
		record := RequestRecord{Binding: binding, CreatedAt: created.Format(time.RFC3339Nano), Deadline: created.Add(want).Format(time.RFC3339Nano)}
		if _, err := validateRequestTiming(record); err != nil {
			t.Fatal(err)
		}
		if revision == provider.ModelsRevision {
			record.Deadline = created.Add(legacyModelDiscoveryTimeout).Format(time.RFC3339Nano)
			if _, err := validateRequestTiming(record); err != nil {
				t.Fatalf("legacy model discovery deadline rejected: %v", err)
			}
		} else {
			record.Deadline = created.Add(legacyAdmissionTimeout).Format(time.RFC3339Nano)
			if _, err := validateRequestTiming(record); err != nil {
				t.Fatalf("legacy admission deadline rejected: %v", err)
			}
		}
		record.Deadline = created.Add(want + time.Second).Format(time.RFC3339Nano)
		if _, err := validateRequestTiming(record); err == nil {
			t.Fatal("accepted modified deadline")
		}
	}
}
