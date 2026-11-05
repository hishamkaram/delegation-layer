package control

import "errors"

// This per-owner set only deduplicates actual command handles. Completion and
// its result remain authoritative in Pending; this owner never calls Wait.
type bootstrapPendingResults struct {
	pending []*Pending
	seen    map[*Pending]struct{}
	err     error
}

func newBootstrapPendingResults(pending []*Pending) *bootstrapPendingResults {
	results := &bootstrapPendingResults{seen: make(map[*Pending]struct{})}
	results.add(pending)
	return results
}

func (r *bootstrapPendingResults) add(pending []*Pending) {
	for _, handle := range pending {
		if handle == nil {
			continue
		}
		if _, seen := r.seen[handle]; !seen {
			r.seen[handle] = struct{}{}
			r.pending = append(r.pending, handle)
		}
	}
}

func (r *bootstrapPendingResults) record(result CommandResult) {
	r.err = errors.Join(r.err, result.Err)
	r.add(pendingHandles(result.Err))
}

func (r *bootstrapPendingResults) collectSettled() {
	var waiting []*Pending
	for len(r.pending) > 0 {
		handle := r.pending[0]
		r.pending = r.pending[1:]
		if result, complete := handle.Result(); complete {
			r.record(result)
		} else {
			waiting = append(waiting, handle)
		}
	}
	r.pending = waiting
}

func (r *bootstrapPendingResults) wait() {
	for len(r.pending) > 0 {
		handle := r.pending[0]
		r.pending = r.pending[1:]
		<-handle.Done()
		if result, complete := handle.Result(); complete {
			r.record(result)
		}
	}
}
