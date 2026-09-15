package fakeprovider

import (
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	// Provider is the explicit identity of the finite provider used by the
	// supervisor harness.
	Provider = "fixture:test"
	// Mode is the only permission mode supported by this fixture.
	Mode = "read-only"
	// Version is the version of the JSONL predicate contract below.
	Version = "2"
)

// fixturePredicateV2Contract is immutable protocol text. Any rule change must
// introduce a new predicate version while retaining this implementation.
const fixturePredicateV2Contract = `fixture:test/read-only/v2
stdout is newline-terminated JSONL; every frame excluding LF is at most 65536 bytes
empty frames, missing final LF, invalid UTF-8, non-object roots, duplicate keys, unknown fields, wrong-case fields, missing fields, null values, trailing JSON and wrong field types are malformed_output
supported objects are session(type,task_id,session_id), answer_chunk(type,task_id,text), and result(type,task_id,session_id,success)
all task_id values equal the sealed task ID
session_id matches [A-Za-z0-9][A-Za-z0-9._:-]{0,127}
exactly one session precedes one final result; answer chunks may precede a late session; events after result are invalid
session and result identities agree with each other, the required expected session, and the recorded fixture identity
answer chunks concatenate decoded JSON text exactly, without trimming, inserted newlines, or a total answer limit
success requires a non-whitespace rune by unicode.IsSpace, a session, a final success=true result, clean exit, and complete stdout/stderr reads
stderr is arbitrary except the ASCII marker DELEGATE_FIXTURE_ERROR, which rejects after complete draining
refusal precedence is start_failed, capture_error, provider_exit, stderr_error, first stdout semantic fault, missing_session, missing_result, empty_answer
stdout semantic reasons are malformed_output, task_identity_mismatch, session_identity_mismatch, duplicate_session, event_after_result, and provider_failure
`

// Contract returns the immutable bytes that are hashed into Reference.
func Contract() string { return fixturePredicateV2Contract }

// ContractDigest returns the lowercase SHA-256 digest bound into Reference.
func ContractDigest() string { return task.ComputeSHA256([]byte(fixturePredicateV2Contract)) }
