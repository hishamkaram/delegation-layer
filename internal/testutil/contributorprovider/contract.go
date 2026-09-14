// Package contributorprovider is a finite provider and protocol fixture used
// by the contributor-proof acceptance exercise. It is deliberately not
// registered in the production catalog.
package contributorprovider

import (
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/testutil/contributorprovider/protocol"
)

const (
	// Provider is a structurally valid test-only provider ID. It must never be
	// added to app.NativeCatalog.
	Provider = "synthetic:contributor-proof"
	Mode     = "read-only"
	Version  = "1"
	Protocol = protocol.Protocol

	RuntimeInputName = protocol.RuntimeInputName
	ToolsInputName   = protocol.ToolsInputName
	OutputName       = protocol.OutputName

	CasePresent      = protocol.CasePresent
	CaseAbsent       = protocol.CaseAbsent
	CaseEmpty        = protocol.CaseEmpty
	CaseConflict     = protocol.CaseConflict
	CaseRejected     = protocol.CaseRejected
	CaseMalformed    = protocol.CaseMalformed
	CaseWrongTask    = protocol.CaseWrongTask
	CaseWrongSession = protocol.CaseWrongSession
	CaseResume       = protocol.CaseResume
	CaseNonzero      = protocol.CaseNonzero
)

// MaxEnvelopeBytes bounds this synthetic protocol's one-turn stdout record.
// The core still bounds and seals the complete raw evidence independently.
const MaxEnvelopeBytes = protocol.MaxEnvelopeBytes

// MaxBriefBytes bounds the finite JSON request consumed from stdin.
const MaxBriefBytes = protocol.MaxBriefBytes

// OutputWriterContract is the shared core's certified completion contract for
// the optional answer artifact. A future registration must certify the same
// value alongside this predicate revision.
const OutputWriterContract = task.OutputWriterProcessExitEOF

// contractV1 is immutable interpretation policy. Any semantic change needs a
// new protocol/predicate revision while this implementation remains available
// for collection of sealed v1 evidence.
const contractV1 = `{"adapter":"synthetic:contributor-proof","mode":"read-only","version":"1","protocol":"contributor-proof/v1","envelope":{"fields":"protocol,task_id,session_id,status,answer; one strict JSON object; no unknown or duplicate fields","status":"complete|rejected","answer":"UTF-8 string"},"brief":{"fields":"case,answer?,nonce?","stdin":"one bounded strict JSON object followed by EOF"},"identity":"envelope task_id equals seal task_id; session_id equals expected and recorded session when supplied","success":"status=complete,exit_code=0,answer non-whitespace; absent answer.txt uses envelope answer; present answer.txt must be nonempty and byte-identical","failure":"rejected status or nonzero exit; empty or conflicting answer.txt rejects; malformed envelope and identity mismatch reject","runtime":"create-once nonsecret launch/completion receipts and private per-session nonce file"}` + "\n"

// Contract returns the canonical bytes whose digest identifies the predicate.
func Contract() string { return contractV1 }

// ContractDigest returns the lowercase SHA-256 digest bound into Reference.
func ContractDigest() string { return task.ComputeSHA256([]byte(contractV1)) }

// Reference returns the exact predicate reference for the synthetic envelope.
func Reference() task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: Mode, Version: Version, SHA256: ContractDigest()}
}

// Brief is the finite JSON request sent to the fixture over stdin. Answer and
// nonce are omitted on a resume case so the provider must recover the nonce
// from its private runtime session file.
type Brief = protocol.Brief

// Envelope is the complete one-turn stdout record. Strict decoding rejects
// missing, duplicate, unknown, trailing, and incorrectly typed fields.
type Envelope = protocol.Envelope

// RuntimeConfig is the non-secret provider runtime configuration materialized
// by the core. The directory must be outside the workspace and task state.
type RuntimeConfig = protocol.RuntimeConfig

// ToolsConfig is the minimal non-secret policy configuration materialized by
// the core. The fixture performs no tool operation; workspace writes are
// always forbidden.
type ToolsConfig = protocol.ToolsConfig
