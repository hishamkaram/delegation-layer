package contract

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// CheckpointCommitPayloadLimit bounds the stored commit object body.
const CheckpointCommitPayloadLimit = 4096

// Checkpoint identity trailers. They are the final two message lines of every
// checkpoint commit payload and appear nowhere else in it.
const (
	CheckpointTrailer        = "Delegate-Checkpoint: "
	CheckpointRequestTrailer = "Delegate-Checkpoint-Request: "
)

// validateCheckpointPayload parses the stored commit body without hashing it.
// Its header is exactly tree, the sole parent, author and committer, bound to
// the candidate's tree and observed HEAD; its message ends with the
// checkpoint ID and request digest trailers.
func validateCheckpointPayload(candidate CheckpointCandidate) error {
	payload := candidate.CommitPayload
	if len(payload) == 0 || len(payload) > CheckpointCommitPayloadLimit || !checkpointPayloadASCII(payload) || !strings.HasSuffix(payload, "\n") {
		return task.ErrIdentityMismatch
	}
	header, message, found := strings.Cut(payload, "\n\n")
	if !found {
		return task.ErrIdentityMismatch
	}
	lines := strings.Split(header, "\n")
	if len(lines) != 4 || lines[0] != "tree "+candidate.TreeOID || lines[1] != "parent "+candidate.ObservedHEAD ||
		!checkpointSignature(lines[2], "author ") || !checkpointSignature(lines[3], "committer ") {
		return task.ErrIdentityMismatch
	}
	return validateCheckpointTrailers(message, candidate)
}

func checkpointPayloadASCII(payload string) bool {
	for index := range len(payload) {
		character := payload[index]
		if character != '\n' && (character < 0x20 || character > 0x7e) {
			return false
		}
	}
	return true
}

func checkpointSignature(line, prefix string) bool {
	identity, found := strings.CutPrefix(line, prefix)
	return found && strings.TrimSpace(identity) != ""
}

func validateCheckpointTrailers(message string, candidate CheckpointCandidate) error {
	lines := strings.Split(strings.TrimSuffix(message, "\n"), "\n")
	count := len(lines)
	if count < 2 || lines[count-2] != CheckpointTrailer+candidate.OperationID || lines[count-1] != CheckpointRequestTrailer+candidate.RequestSHA256 {
		return task.ErrIdentityMismatch
	}
	for _, line := range lines[:count-2] {
		if strings.HasPrefix(line, strings.TrimSuffix(CheckpointTrailer, ": ")) {
			return task.ErrIdentityMismatch
		}
	}
	return nil
}
