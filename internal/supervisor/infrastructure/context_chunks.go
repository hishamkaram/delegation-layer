package infrastructure

import (
	"bytes"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// contextChunkBytes is a format constant: changing boundaries changes manifest
// identities and requires a protocol bump. Native exec owns real feasibility.
const contextChunkBytes = 512 * 1024

type contextManifest struct {
	Protocol             int    `json:"protocol"`
	Root                 string `json:"root"`
	RootID               string `json:"root_id"`
	OS                   string `json:"os"`
	SupervisorExecutable string `json:"supervisor_executable"`
	ControlBytes         int64  `json:"control_bytes"`
	MaintenanceBytes     int64  `json:"maintenance_bytes"`
	Head                 string `json:"head"`
}

type contextChunk struct {
	Protocol int    `json:"protocol"`
	Data     []byte `json:"data"`
	Next     string `json:"next"`
}

func environmentBytes(environment []string) (int64, error) {
	var size int64
	maximum := int64(int(^uint(0) >> 1))
	for _, entry := range environment {
		length := int64(len(entry))
		if length >= maximum-size {
			return 0, ErrIdentity
		}
		size += length + 1
	}
	return size, nil
}

// Check counts before summing or converting; no declared count allocates memory.
func (m contextManifest) payloadBytes() (int, error) {
	maximum := int64(int(^uint(0) >> 1))
	if m.ControlBytes < 0 || m.MaintenanceBytes < 0 || m.MaintenanceBytes > maximum || m.ControlBytes > maximum-m.MaintenanceBytes {
		return 0, ErrIdentity
	}
	return int(m.ControlBytes + m.MaintenanceBytes), nil
}

func contextChunkCount(total int) int {
	if total == 0 {
		return 0
	}
	return 1 + (total-1)/contextChunkBytes
}

func (m contextManifest) validate() error {
	if m.Protocol != RecordProtocolVersion || (m.OS != "linux" && m.OS != "darwin") {
		return ErrIdentity
	}
	if !cleanAbsolutePath(m.SupervisorExecutable) {
		return ErrIdentity
	}
	if _, err := NewOrigin(m.Root, m.RootID); err != nil {
		return err
	}
	total, err := m.payloadBytes()
	if err != nil {
		return err
	}
	if total == 0 {
		if m.Head != "" {
			return ErrIdentity
		}
	} else if task.ValidateSHA256(m.Head) != nil {
		return ErrIdentity
	}

	return nil
}

func encodeLaunchContext(record LaunchContext) (contextManifest, []contextChunk, error) {
	if err := record.Validate(); err != nil {
		return contextManifest{}, nil, err
	}
	control, err := environmentBytes(record.ControlEnvironment)
	if err != nil {
		return contextManifest{}, nil, err
	}
	maintenance, err := environmentBytes(record.MaintenanceEnvironment)
	if err != nil {
		return contextManifest{}, nil, err
	}
	manifest := contextManifest{Protocol: record.Protocol, Root: record.Root, RootID: record.RootID, OS: record.OS, SupervisorExecutable: record.SupervisorExecutable, ControlBytes: control, MaintenanceBytes: maintenance}
	total, err := manifest.payloadBytes()
	if err != nil {
		return contextManifest{}, nil, err
	}
	payload := make([]byte, 0, total)
	for _, role := range [][]string{record.ControlEnvironment, record.MaintenanceEnvironment} {
		for _, entry := range role {
			payload = append(payload, entry...)
			payload = append(payload, 0)
		}
	}
	// Build from the tail so each immutable digest includes its successor.
	// Return payload order; the writer publishes this slice in reverse.
	chunks := make([]contextChunk, contextChunkCount(total))
	next := ""
	for index := len(chunks) - 1; index >= 0; index-- {
		offset := index * contextChunkBytes
		chunk := contextChunk{Protocol: RecordProtocolVersion, Data: payload[offset : offset+min(contextChunkBytes, total-offset)], Next: next}
		digest, digestErr := digestStartupRecord(chunk)
		if digestErr != nil {
			return contextManifest{}, nil, digestErr
		}
		chunks[index] = chunk
		next = digest
	}
	manifest.Head = next
	return manifest, chunks, nil
}

func decodeContextPayload(manifest contextManifest, payload []byte) (LaunchContext, error) {
	if err := manifest.validate(); err != nil {
		return LaunchContext{}, err
	}
	if int64(len(payload)) != manifest.ControlBytes+manifest.MaintenanceBytes {
		return LaunchContext{}, ErrIdentity
	}
	control, err := splitEnvironment(payload[:manifest.ControlBytes])
	if err != nil {
		return LaunchContext{}, err
	}
	maintenance, err := splitEnvironment(payload[manifest.ControlBytes:])
	if err != nil {
		return LaunchContext{}, err
	}
	record := LaunchContext{Protocol: manifest.Protocol, Root: manifest.Root, RootID: manifest.RootID, OS: manifest.OS, SupervisorExecutable: manifest.SupervisorExecutable, ControlEnvironment: control, MaintenanceEnvironment: maintenance}
	if err = record.Validate(); err != nil {
		return LaunchContext{}, err
	}
	return record, nil
}

func splitEnvironment(payload []byte) ([]string, error) {
	if len(payload) == 0 {
		return []string{}, nil
	}
	if payload[len(payload)-1] != 0 {
		return nil, ErrIdentity
	}
	entries := bytes.Split(payload[:len(payload)-1], []byte{0})
	result := make([]string, len(entries))
	for index, entry := range entries {
		result[index] = string(entry)
	}
	return result, nil
}
