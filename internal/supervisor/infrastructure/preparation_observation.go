package infrastructure

import (
	"context"
	"os"
	"sync"
)

// PreparationObservation owns one operation's observed outcome, not native
// authority. Fixed host composition records canonical readiness; only a real
// successful Start callback records Started. Its borrowed guard remains the
// sole native authority. Close/registration errors must not erase either fact.
type PreparationObservation struct {
	mu      sync.Mutex
	guard   DaemonStartGuard
	context LaunchContextReference
	started bool
	ready   bool
}

func NewPreparationObservation(guard DaemonStartGuard, selected LaunchContextReference) (*PreparationObservation, error) {
	if guard == nil {
		return nil, os.ErrInvalid
	}
	if err := selected.Validate(); err != nil {
		return nil, err
	}
	return &PreparationObservation{guard: guard, context: selected}, nil
}

// GuardDaemonStart preserves the exact callback and guard errors. The canonical
// Pueue start owner enforces synchronous, at-most-once callback use and owns Wait.
func (o *PreparationObservation) GuardDaemonStart(ctx context.Context, root string, start func() error) error {
	if o == nil || o.guard == nil || ctx == nil || start == nil {
		return os.ErrInvalid
	}
	return o.guard.GuardDaemonStart(ctx, root, func() error {
		if err := start(); err != nil {
			return err
		}
		o.mu.Lock()
		o.started = true
		o.mu.Unlock()
		return nil
	})
}

// RecordReady is called only after the canonical preparation consumer verified
// readiness, before closing its client. It never infers readiness from Start,
// Wait registration, a publication record or the absence of an error alone.
func (o *PreparationObservation) RecordReady() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ready = true
}

// Disposition retains partial success even when the operation also reports a
// cleanup/control error. Zero means no verified outcome; it is not WarmReuse.
func (o *PreparationObservation) Disposition() Disposition {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.started {
		return Disposition{Kind: DispositionStarted, Context: o.context}
	}
	if o.ready {
		return Disposition{Kind: DispositionWarmReuse}
	}
	return Disposition{}
}
