package control

import (
	"log"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
)

// ReportDiagnostic emits best-effort maintenance telemetry through the
// selected invocation sink. It grants no lifecycle authority.
func (c *Client) ReportDiagnostic(err error) { reportDiagnostic(c.options.ReportDiagnostic, err) }

func reportDiagnostic(report func(error), err error) {
	if err == nil {
		return
	}
	if report != nil {
		report(err)
		return
	}
	log.Printf("%v", err)
}

// Pending commands carry their original invocation's sink through deferred
// cleanup, avoiding globals and preserving causal ownership after callers exit.
func reportRetained(pending []*Pending, err error) {
	for _, command := range pending {
		if report := transport.PendingDiagnosticReporter(command); report != nil {
			reportDiagnostic(report, err)
			return
		}
	}
	reportDiagnostic(nil, err)
}
