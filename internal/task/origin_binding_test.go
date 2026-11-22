package task

import (
	"errors"
	"strings"
	"testing"
)

const legacyOriginJSON = `{"gateway_id":"gateway","admission_generation":"generation","operation_id":"operation","principal_ref":"principal","gateway_request_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`

const originBindingTaskSuffix = `,"connector_context":{"root_token":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","project":{"path":"/fixture/workspace","device":7,"inode":19}},"schema_version":1,"root_id":"11111111111111111111111111111111","task_id":"22222222222222222222222222222222","provider":"fixture:test","mode":"read-only","canonical_cwd":"/fixture/workspace","requested_config":{"permission":"read-only","budget":"1s"},"budget_nanos":1000000000,"brief_sha256":"29a8825bd242f14386ee528d76e0e8f1e38f3c8c4047d7b2d6df7493368a17d0","brief_length":5}` + "\n"

const legacyOriginTaskJSON = `{"origin":` + legacyOriginJSON + originBindingTaskSuffix

// Frozen from the literal legacy request bytes, independently of MarshalCanonical.
const legacyOriginTaskSpecSHA256 = "6597a70230659b12a336c9625e96adebac030a5e1b209098ae23d61680520e78"

func decodeOriginBindingTask(t *testing.T, wire string) TaskRecord {
	t.Helper()
	var record TaskRecord
	if err := DecodeStrict([]byte(wire), &record); err != nil {
		t.Fatalf("strict task decode: %v", err)
	}
	if err := ValidateTaskRecord(&record); err != nil {
		t.Fatalf("task validation: %v", err)
	}
	return record
}

func TestGatewayOriginBindingRawJSONRoundTrip(t *testing.T) {
	// The raw wire input intentionally compiles against the historic type. Its
	// opaque string is neither a parsed Gateway object nor restricted text.
	origin := strings.TrimSuffix(legacyOriginJSON, "}") + `,"binding":" future:v9\n\u0001雪\t "}`
	wire := `{"origin":` + origin + originBindingTaskSuffix
	first := decodeOriginBindingTask(t, wire)
	encoded, err := MarshalCanonical(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != wire {
		t.Fatal("canonical task bytes lost or changed the opaque origin binding")
	}
	cloned := first.Origin.Clone()
	if cloned == nil || cloned == first.Origin || *cloned != *first.Origin {
		t.Fatal("origin clone did not preserve an independent equal value")
	}
	second := decodeOriginBindingTask(t, wire)
	if *second.Origin != *cloned || !CompareRequests(&first, &second) {
		t.Fatal("separately decoded binding changed origin or request equality")
	}
	if ComputeSHA256(encoded) == legacyOriginTaskSpecSHA256 {
		t.Fatal("nonempty origin binding did not affect the task spec digest")
	}
}

func TestGatewayOriginLegacyCanonicalBytesAndSpec(t *testing.T) {
	record := decodeOriginBindingTask(t, legacyOriginTaskJSON)
	encoded, err := MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != legacyOriginTaskJSON || ComputeSHA256(encoded) != legacyOriginTaskSpecSHA256 {
		t.Fatal("absent origin binding changed historic canonical task bytes or spec digest")
	}
}

func TestGatewayOriginBindingEmptyUsesLegacySpec(t *testing.T) {
	origin := strings.TrimSuffix(legacyOriginJSON, "}") + `,"binding":""}`
	record := decodeOriginBindingTask(t, `{"origin":`+origin+originBindingTaskSuffix)
	encoded, err := MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != legacyOriginTaskJSON || ComputeSHA256(encoded) != legacyOriginTaskSpecSHA256 {
		t.Fatal("empty origin binding changed historic canonical task bytes or spec digest")
	}
}

func TestGatewayOriginBindingValidationIsOpaqueAndByteBounded(t *testing.T) {
	record := decodeOriginBindingTask(t, legacyOriginTaskJSON)
	for _, binding := range []string{"", " future:v9\r\n\t\x01\x7f\u0085雪 ", strings.Repeat("x", MaxControlRecordSize), strings.Repeat("é", MaxControlRecordSize/2)} {
		origin := *record.Origin
		origin.Binding = binding
		if err := ValidateGatewayOrigin(origin); err != nil {
			t.Fatal("valid opaque UTF-8 binding was restricted:", err)
		}
	}
	for _, binding := range []string{string([]byte{0xff}), "embedded\x00nul", strings.Repeat("x", MaxControlRecordSize+1), strings.Repeat("雪", MaxControlRecordSize/3+1)} {
		origin := *record.Origin
		origin.Binding = binding
		if err := ValidateGatewayOrigin(origin); err == nil {
			t.Fatal("invalid UTF-8, NUL, or oversized binding was accepted")
		}
	}
}

func TestGatewayOriginBindingChangesIdentityWithoutAliasingClone(t *testing.T) {
	legacy := decodeOriginBindingTask(t, legacyOriginTaskJSON)
	bound := legacy
	bound.Origin = legacy.Origin.Clone()
	bound.Origin.Binding = "opaque:first"
	if *bound.Origin == *legacy.Origin || CompareRequests(&bound, &legacy) || legacy.Origin.Binding != "" {
		t.Fatal("binding did not participate in immutable origin or request identity")
	}
	cloned := bound.Origin.Clone()
	cloned.Binding = "opaque:second"
	if bound.Origin.Binding != "opaque:first" || *cloned == *bound.Origin {
		t.Fatal("changing a cloned binding changed the original or preserved equality")
	}
}

func TestGatewayOriginBindingKeepsEncodedControlRecordBound(t *testing.T) {
	record := decodeOriginBindingTask(t, legacyOriginTaskJSON)
	// Escaping expands this valid, sub-limit opaque string past the existing
	// encoded-record bound. The strict record owner must still reject it.
	record.Origin.Binding = strings.Repeat("\x01", MaxControlRecordSize/2)
	if err := ValidateGatewayOrigin(*record.Origin); err != nil {
		t.Fatal("raw binding bytes exceeded their field bound unexpectedly:", err)
	}
	encoded, err := MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) <= MaxControlRecordSize {
		t.Fatal("fixture did not cross the existing encoded-record bound")
	}
	var decoded TaskRecord
	if err = DecodeStrict(encoded, &decoded); !errors.Is(err, ErrControlRecordTooBig) {
		t.Fatalf("encoded record bypassed its existing owner bound: %v", err)
	}
}
