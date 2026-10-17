package app

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestCollectionOptionsReportMaintenanceDeferralOncePerInvocation(t *testing.T) {
	var diagnostics bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&diagnostics)
	t.Cleanup(func() { log.SetOutput(previous) })
	deps := storeDependencies{observeSupervisor: true}
	options := deps.collectionOptions(time.Minute)
	if options.Watch != time.Minute || !options.ObserveSupervisor || options.ReportMaintenanceDeferral == nil {
		t.Fatal("collection lost its observation options or diagnostic reporter")
	}
	options.ReportMaintenanceDeferral(nil)
	options.ReportMaintenanceDeferral(pueue.ErrPrivateSupervisorTransitionIncomplete)
	options.ReportMaintenanceDeferral(pueue.ErrPrivateSupervisorTransitionIncomplete)
	log.SetOutput(previous)
	if count := strings.Count(diagnostics.String(), "task supervisor maintenance deferred"); count != 1 {
		t.Errorf("collection reported the same nonfatal deferral %d times: %s", count, &diagnostics)
	}
	if !strings.Contains(diagnostics.String(), pueue.ErrPrivateSupervisorTransitionIncomplete.Error()) {
		t.Errorf("collection discarded the refusal cause: %s", &diagnostics)
	}
}
