package infrastructure

type DispositionKind string

const (
	DispositionStarted   DispositionKind = "started"
	DispositionWarmReuse DispositionKind = "warm-reuse"
)

// Disposition reports the resident's observed preparation outcome. The request
// handler owns its construction: Started requires a successful real Start
// callback and its resolved context; WarmReuse requires canonical readiness
// without a Start. A partial failure after Start retains Started alongside the
// error. This value alone proves neither readiness nor native ownership.
type Disposition struct {
	Kind    DispositionKind        `json:"kind"`
	Context LaunchContextReference `json:"context"`
}

func (d Disposition) Validate() error {
	switch d.Kind {
	case DispositionStarted:
		return d.Context.Validate()
	case DispositionWarmReuse:
		if d.Context != (LaunchContextReference{}) {
			return ErrIdentity
		}
		return nil
	default:
		return ErrIdentity
	}
}
