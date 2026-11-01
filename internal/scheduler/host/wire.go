package host

import "time"

// This DTO is a serialization boundary, never a second state owner. task's
// required-field codec expects structs to be JSON objects; native time.Time is
// a JSON string. Keep that representation explicit and parse it at the edge.
type wireStatus struct {
	State        string    `json:"state"`
	Epoch        string    `json:"epoch,omitempty"`
	Attempt      uint64    `json:"attempt,omitempty"`
	Ready        bool      `json:"ready"`
	Fault        bool      `json:"fault,omitempty"`
	NextRetryAt  string    `json:"next_retry_at,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	StopAccepted bool      `json:"stop_accepted"`
	Protocol     int       `json:"protocol"`
	BuildID      string    `json:"build_id,omitempty"`
	Host         Identity  `json:"host"`
	Child        *Identity `json:"child,omitempty"`
}

func statusWire(status Status) wireStatus {
	wire := wireStatus{
		State: status.State, Epoch: status.Epoch, Attempt: status.Attempt, Ready: status.Ready, Fault: status.Fault,
		LastError: status.LastError, StopAccepted: status.StopAccepted, Protocol: status.Protocol,
		BuildID: status.BuildID, Host: status.Host, Child: status.Child,
	}
	if status.NextRetryAt != nil {
		wire.NextRetryAt = status.NextRetryAt.UTC().Format(time.RFC3339Nano)
	}
	return wire
}

func (wire wireStatus) value() (Status, error) {
	status := Status{
		State: wire.State, Epoch: wire.Epoch, Attempt: wire.Attempt, Ready: wire.Ready, Fault: wire.Fault,
		LastError: wire.LastError, StopAccepted: wire.StopAccepted, Protocol: wire.Protocol,
		BuildID: wire.BuildID, Host: wire.Host, Child: wire.Child,
	}
	if wire.NextRetryAt != "" {
		instant, err := time.Parse(time.RFC3339Nano, wire.NextRetryAt)
		if err != nil {
			return Status{}, err
		}
		status.NextRetryAt = &instant
	}
	return status, nil
}
