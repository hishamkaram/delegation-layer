package control

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue/protocol"
)

const (
	MaxControlBytes       = protocol.MaxControlBytes
	inspectionGroupPrefix = protocol.InspectionGroupPrefix
)

var (
	ErrConfiguration = protocol.ErrConfiguration
	ErrBinding       = protocol.ErrBinding
	ErrUnknown       = protocol.ErrUnknown
	ErrControlLimit  = protocol.ErrControlLimit
)

type (
	Config                   = protocol.Config
	ClientConfig             = protocol.ClientConfig
	DaemonConfig             = protocol.DaemonConfig
	SharedConfig             = protocol.SharedConfig
	Identity                 = protocol.Identity
	Launch                   = protocol.Launch
	State                    = protocol.State
	Job                      = protocol.Job
	Group                    = protocol.Group
	QueueSnapshot            = protocol.QueueSnapshot
	CommandEvent             = protocol.CommandEvent
	InspectionIdentity       = protocol.InspectionIdentity
	TerminalEvent            = protocol.TerminalEvent
	RunnerCommandReplacement = protocol.RunnerCommandReplacement
)

const (
	StateUnknown = protocol.StateUnknown
	StateQueued  = protocol.StateQueued
	StateStashed = protocol.StateStashed
	StateRunning = protocol.StateRunning
	StateEnded   = protocol.StateEnded
)

func inspectionGroup(rootID string) string { return protocol.InspectionGroup(rootID) }

func originalCommand(job Job) string { return protocol.OriginalCommand(job) }

func cloneQueueSnapshot(snapshot QueueSnapshot) QueueSnapshot {
	return protocol.CloneQueueSnapshot(snapshot)
}

func ParseConfig(data []byte) (*Config, error) { return protocol.ParseConfig(data) }

func ParseStatus(data []byte, version string) ([]Job, error) {
	return protocol.ParseStatus(data, version)
}

func ParseQueueSnapshot(data []byte, version string) (QueueSnapshot, error) {
	return protocol.ParseQueueSnapshot(data, version)
}

func configWithFileEditMode(data []byte) ([]byte, error) {
	return protocol.ConfigWithFileEditMode(data)
}

func configWithSocketPath(data []byte, socketPath string) ([]byte, error) {
	return protocol.ConfigWithSocketPath(data, socketPath)
}

func parseID(value string) (int64, error) { return protocol.ParseID(value) }

func valueOr[T any](value *T, fallback T) T { return protocol.ValueOr(value, fallback) }

func jobWithOriginalCommand(job Job, command string) Job {
	return protocol.WithOriginalCommand(job, command)
}
