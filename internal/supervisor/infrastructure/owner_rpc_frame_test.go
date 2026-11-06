package infrastructure

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	ownerWireEmptySource          = `"source":{"root_id":"","kind":"","owner_id":"","record_id":"","binding_sha256":""}`
	ownerWireEmptyRequestSubjects = `,` + ownerWireEmptySource + `}`
	ownerWireEmptyReplySubjects   = `,"context":{"sha256":""},` + ownerWireEmptySource + `,"disposition":{"kind":"","context":{"sha256":""}}}`
	ownerWireRoot                 = "0123456789abcdef0123456789abcdef"
	ownerWireDigest               = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ownerWireRequestJSON          = `{"protocol":4,"root_id":"` + ownerWireRoot + `","owner":{"sequence":1,"epoch":"` + ownerWireRoot + `","intent_sha256":"` + ownerWireDigest + `"},"publication_sha256":"` + ownerWireDigest + `","client":{"pid":7,"start_token":"fixture:7"},"action":"status","target":{"root":"","root_id":""},"context":{"sha256":""},"deadline":"2026-11-03T00:00:30Z"` + ownerWireEmptyRequestSubjects + "\n"
	ownerWireReplyJSON            = `{"protocol":4,"root_id":"` + ownerWireRoot + `","owner_ref":{"sequence":1,"epoch":"` + ownerWireRoot + `","intent_sha256":"` + ownerWireDigest + `"},"publication_sha256":"` + ownerWireDigest + `","action":"status","target":{"root":"","root_id":""},"startup":{"sequence":0,"epoch":"","intent_sha256":""},"code":"ok"` + ownerWireEmptyReplySubjects + "\n"
)

func ownerRequestFixture() ownerRequest {
	return ownerRequest{Protocol: ownerProtocolVersion, RootID: ownerWireRoot, Owner: StartupReference{Sequence: 1, Epoch: ownerWireRoot, IntentSHA256: ownerWireDigest}, PublicationSHA256: ownerWireDigest, Client: ProcessIdentity{PID: 7, StartToken: "fixture:7"}, Action: ownerStatus, Deadline: "2026-11-03T00:00:30Z"}
}

func ownerReplyFixture() ownerReply {
	return ownerReply{Protocol: ownerProtocolVersion, RootID: ownerWireRoot, OwnerRef: StartupReference{Sequence: 1, Epoch: ownerWireRoot, IntentSHA256: ownerWireDigest}, PublicationSHA256: ownerWireDigest, Action: ownerStatus, Code: ownerOK}
}

func TestOwnerFramesMatchIndependentWireFixtures(t *testing.T) {
	request, err := readOwnerFrame[ownerRequest](bufio.NewReader(strings.NewReader(ownerWireRequestJSON)))
	if err != nil || request != ownerRequestFixture() {
		t.Fatalf("request fixture: %v", err)
	}
	reply, err := readOwnerFrame[ownerReply](bufio.NewReader(strings.NewReader(ownerWireReplyJSON)))
	if err != nil || reply != ownerReplyFixture() {
		t.Fatalf("reply fixture: %v", err)
	}
	var output bytes.Buffer
	if err := writeOwnerFrame(&output, ownerRequestFixture()); err != nil {
		t.Fatal(err)
	}
	if output.String() != ownerWireRequestJSON {
		t.Fatal("request canonical bytes changed")
	}
	output.Reset()
	if err := writeOwnerFrame(&output, ownerReplyFixture()); err != nil {
		t.Fatal(err)
	}
	if output.String() != ownerWireReplyJSON {
		t.Fatal("reply canonical bytes changed")
	}
}

func TestOwnerRequestStrictRejectionsReturnZero(t *testing.T) {
	for _, replacement := range [][2]string{
		{`"protocol":4`, `"protocol":1`},
		{`"protocol":4,`, ``},
		{`"protocol":4`, `"Protocol":1`},
		{`"protocol":4`, `"protocol":4,"protocol":4`},
		{`"protocol":4`, `"protocol":4,"unknown":"sensitive"`},
		{`"client":{`, `"client":{"unknown":"sensitive",`},
		{`"pid":7`, `"pid":0`},
		{`"sequence":1`, `"sequence":0`},
		{`"epoch":"` + ownerWireRoot + `"`, `"epoch":"invalid"`},
		{`"publication_sha256":"` + ownerWireDigest + `"`, `"publication_sha256":"bad"`},
		{`"client":{"pid":7,"start_token":"fixture:7"}`, `"client":null`},
		{`"action":"status"`, `"action":"upgrade"`},
		{`"action":"status"`, `"action":"migrate-referenced"`},
		{`"action":"status"`, `"action":"status","migration":{}`},
		{`"action":"status"`, `"action":"status","migration":null`},
		{`"deadline":"2026-11-03T00:00:30Z"`, `"deadline":"2026-11-03T00:00:30+00:00"`},
		{`"deadline":"2026-11-03T00:00:30Z"`, `"deadline":"0001-01-01T00:00:00Z"`},
		{`"target":{"root":"","root_id":""}`, `"target":null`},
		{`"target":{"root":"","root_id":""}`, `"target":{"root":"relative","root_id":"bad"}`},
		{`"context":{"sha256":""}`, `"context":{"sha256":"` + ownerWireDigest + `"}`},
	} {
		input := strings.Replace(ownerWireRequestJSON, replacement[0], replacement[1], 1)
		got, err := readOwnerFrame[ownerRequest](bufio.NewReaderSize(strings.NewReader(input), 17))
		if err == nil || !errors.Is(err, ErrIdentity) || errors.Is(err, task.ErrEvidenceFault) || got != (ownerRequest{}) {
			t.Fatalf("strict rejection %q: zero=%t err=%v", replacement[0], got == (ownerRequest{}), err)
		}
	}
}

func TestOwnerFramesRejectPreviousPreparationCodec(t *testing.T) {
	for _, wire := range []string{ownerWireRequestJSON, ownerWireReplyJSON} {
		previous := strings.Replace(wire, `"protocol":4`, `"protocol":3`, 1)
		previous = strings.TrimSuffix(strings.TrimSpace(previous), "}") + `,"migration":{"root_id":"","record_sha256":"","history_index":0}}` + "\n"
		decoded, err := readOwnerFrame[ownerRequest](bufio.NewReader(strings.NewReader(previous)))
		if !errors.Is(err, ErrOwnerIncompatible) || !errors.Is(err, ErrIdentity) || decoded != (ownerRequest{}) {
			t.Fatalf("previous owner codec lost explicit incompatibility: %v", err)
		}
	}
}

func TestOwnerReplyRejectsRemovedMigrationFields(t *testing.T) {
	for _, extra := range []string{`"migration":{}`, `"migration_result":"completed"`} {
		wire := strings.TrimSuffix(strings.TrimSpace(ownerWireReplyJSON), "}") + "," + extra + "}\n"
		reply, err := readOwnerFrame[ownerReply](bufio.NewReader(strings.NewReader(wire)))
		if !errors.Is(err, ErrIdentity) || reply != (ownerReply{}) {
			t.Fatalf("removed migration reply field accepted: %v", err)
		}
	}
}

func TestOwnerFrameMalformedDiagnostics(t *testing.T) {
	for _, input := range []string{"null\n", "{\"sensitive-token\":}\n", strings.TrimSpace(ownerWireRequestJSON) + " {}\n"} {
		got, err := readOwnerFrame[ownerRequest](bufio.NewReader(strings.NewReader(input)))
		if err == nil || got != (ownerRequest{}) || strings.Contains(err.Error(), "sensitive") || !errors.Is(err, ErrIdentity) {
			t.Fatalf("malformed input: zero=%t err=%v", got == (ownerRequest{}), err)
		}
	}
	_, syntaxErr := readOwnerFrame[ownerRequest](bufio.NewReader(strings.NewReader("{\"sensitive-token\":}\n")))
	var syntax *json.SyntaxError
	if !errors.As(syntaxErr, &syntax) {
		t.Fatal("syntax cause lost")
	}
}

func TestOwnerFrameTransportReadFaults(t *testing.T) {
	for _, input := range []string{"", strings.TrimSuffix(ownerWireRequestJSON, "\n"), "{\"protocol\":"} {
		got, err := readOwnerFrame[ownerRequest](bufio.NewReader(strings.NewReader(input)))
		if !errors.Is(err, io.EOF) || errors.Is(err, ErrIdentity) || got != (ownerRequest{}) {
			t.Fatalf("EOF lost: zero=%t err=%v", got == (ownerRequest{}), err)
		}
		if input != "" && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal("truncation cause lost")
		}
	}
	fault := errors.New("read interrupted")
	got, err := readOwnerFrame[ownerRequest](bufio.NewReader(io.MultiReader(strings.NewReader("{"), ownerFaultReader{fault})))
	if !errors.Is(err, fault) || errors.Is(err, ErrIdentity) || got != (ownerRequest{}) {
		t.Fatalf("read cause lost: %v", err)
	}
}

type ownerFaultReader struct{ err error }

func (r ownerFaultReader) Read([]byte) (int, error) { return 0, r.err }

type ownerTestWriter struct {
	short bool
	err   error
	calls int
}

func (w *ownerTestWriter) Write(data []byte) (int, error) {
	w.calls++
	if w.short {
		return len(data) - 1, w.err
	}
	return len(data), w.err
}

func TestOwnerFrameBoundsAndSeparateFrames(t *testing.T) {
	padded := strings.TrimSuffix(ownerWireRequestJSON, "\n") + strings.Repeat(" ", task.MaxControlRecordSize-len(ownerWireRequestJSON)) + "\n"
	got, err := readOwnerFrame[ownerRequest](bufio.NewReaderSize(strings.NewReader(padded), 31))
	if err != nil || got != ownerRequestFixture() {
		t.Fatalf("shared limit rejected: %v", err)
	}
	for _, input := range []string{padded[:len(padded)-1] + " \n", strings.Repeat("x", task.MaxControlRecordSize+1)} {
		got, readErr := readOwnerFrame[ownerRequest](bufio.NewReaderSize(strings.NewReader(input), 31))
		if !errors.Is(readErr, task.ErrControlRecordTooBig) || got != (ownerRequest{}) {
			t.Fatalf("oversize not refused: %v", readErr)
		}
	}
	reader := bufio.NewReader(strings.NewReader(ownerWireRequestJSON + ownerWireReplyJSON))
	first, err := readOwnerFrame[ownerRequest](reader)
	if err != nil || first != ownerRequestFixture() {
		t.Fatal("first frame conflated")
	}
	second, err := readOwnerFrame[ownerReply](reader)
	if err != nil || second != ownerReplyFixture() {
		t.Fatal("second frame lost")
	}
}

func TestOwnerWriterChecksValidationAndShortWrites(t *testing.T) {
	fault := errors.New("write interrupted")
	for _, writer := range []*ownerTestWriter{{short: true}, {err: fault}} {
		err := writeOwnerFrame(writer, ownerRequestFixture())
		expected := fault
		if writer.short {
			expected = io.ErrShortWrite
		}
		if !errors.Is(err, expected) || errors.Is(err, ErrIdentity) {
			t.Fatalf("transport write misclassified or cause lost: %v", err)
		}
	}
	writer := &ownerTestWriter{}
	invalid := ownerRequestFixture()
	invalid.Action = "unknown"
	if err := writeOwnerFrame(writer, invalid); !errors.Is(err, ErrIdentity) || writer.calls != 0 {
		t.Fatal("invalid frame reached writer")
	}
}

func TestOwnerRequestActionSubjects(t *testing.T) {
	target := originRecord{Root: "/target", RootID: strings.Repeat("b", 32)}
	for _, tc := range []struct {
		name    string
		action  ownerAction
		target  originRecord
		context LaunchContextReference
		valid   bool
	}{
		{name: "own", action: ownerEnsureOwn, valid: true},
		{name: "other", action: ownerEnsureOther, target: target, context: LaunchContextReference{SHA256: ownerWireDigest}, valid: true},
		{name: "other missing target", action: ownerEnsureOther, context: LaunchContextReference{SHA256: ownerWireDigest}},
		{name: "other missing context", action: ownerEnsureOther, target: target},
		{name: "other same owner", action: ownerEnsureOther, target: originRecord{Root: "/target", RootID: ownerWireRoot}, context: LaunchContextReference{SHA256: ownerWireDigest}},
		{name: "other malformed path", action: ownerEnsureOther, target: originRecord{Root: "relative", RootID: target.RootID}, context: LaunchContextReference{SHA256: ownerWireDigest}},
		{name: "own selected context", action: ownerEnsureOwn, context: LaunchContextReference{SHA256: ownerWireDigest}, valid: true},
		{name: "status nonzero target", action: ownerStatus, target: target},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := ownerRequestFixture()
			request.Action, request.Target, request.Context = tc.action, tc.target, tc.context
			var wire bytes.Buffer
			err := writeOwnerFrame(&wire, request)
			if (err == nil) != tc.valid {
				t.Fatalf("request action validity = %v", err)
			}
			if tc.valid {
				got, readErr := readOwnerFrame[ownerRequest](bufio.NewReader(&wire))
				if readErr != nil || got != request {
					t.Fatalf("subject changed: %v", readErr)
				}
			}
		})
	}
}

func TestOwnerReplyActionSubjectsAndFiniteCodes(t *testing.T) {
	other := originRecord{Root: "/target", RootID: strings.Repeat("b", 32)}
	owner := ownerReplyFixture().OwnerRef
	for _, tc := range []struct {
		name    string
		action  ownerAction
		target  originRecord
		startup StartupReference
		code    ownerReplyCode
		valid   bool
	}{
		{name: "status", action: ownerStatus, code: ownerOK, valid: true},
		{name: "own accepted", action: ownerEnsureOwn, target: originRecord{Root: "/owner", RootID: ownerWireRoot}, startup: owner, code: ownerOK, valid: true},
		{name: "own pending", action: ownerEnsureOwn, target: originRecord{Root: "/owner", RootID: ownerWireRoot}, startup: owner, code: ownerPending, valid: true},
		{name: "own refused", action: ownerEnsureOwn, code: ownerRefused, valid: true},
		{name: "other accepted", action: ownerEnsureOther, target: other, startup: owner, code: ownerOK, valid: true},
		{name: "other pending", action: ownerEnsureOther, target: other, startup: owner, code: ownerPending, valid: true},
		{name: "other refused", action: ownerEnsureOther, target: other, code: ownerRefused, valid: true},
		{name: "status target", action: ownerStatus, target: other, code: ownerOK},
		{name: "status startup", action: ownerStatus, startup: owner, code: ownerOK},
		{name: "own missing startup", action: ownerEnsureOwn, target: originRecord{Root: "/owner", RootID: ownerWireRoot}, code: ownerOK},
		{name: "own mismatched startup", action: ownerEnsureOwn, target: originRecord{Root: "/owner", RootID: ownerWireRoot}, startup: StartupReference{Sequence: 2, Epoch: owner.Epoch, IntentSHA256: owner.IntentSHA256}, code: ownerOK},
		{name: "other missing startup", action: ownerEnsureOther, target: other, code: ownerPending},
		{name: "unknown code", action: ownerStatus, code: "unbounded-diagnostic"},
		{name: "unknown action", action: "stop", code: ownerOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := ownerReplyFixture()
			reply.Action, reply.Target, reply.Startup, reply.Code = tc.action, tc.target, tc.startup, tc.code
			reply = ownerReplyWithRequiredObservation(reply)
			var wire bytes.Buffer
			err := writeOwnerFrame(&wire, reply)
			if (err == nil) != tc.valid {
				t.Fatalf("reply action validity = %v", err)
			}
			if tc.valid {
				got, readErr := readOwnerFrame[ownerReply](bufio.NewReader(&wire))
				if readErr != nil || got != reply {
					t.Fatalf("subject changed: %v", readErr)
				}
			}
		})
	}
	for _, code := range []ownerReplyCode{ownerOK, ownerPending, ownerBusy, ownerIncompatible, ownerRefused, ownerFault, ownerTimeout} {
		reply := ownerReplyFixture()
		reply.Code = code
		if err := writeOwnerFrame(io.Discard, reply); err != nil {
			t.Fatalf("finite status code %q: %v", code, err)
		}
	}
}

func TestOwnerWriterRejectsOversizeBeforeIO(t *testing.T) {
	request := ownerRequestFixture()
	request.Action = ownerEnsureOther
	request.Target = originRecord{Root: "/" + strings.Repeat("x", task.MaxControlRecordSize), RootID: strings.Repeat("b", 32)}

	request.Context = LaunchContextReference{SHA256: ownerWireDigest}
	writer := &ownerTestWriter{}
	if err := writeOwnerFrame(writer, request); !errors.Is(err, task.ErrControlRecordTooBig) || writer.calls != 0 {
		t.Fatalf("oversize reached writer: %v", err)
	}
}

func ownerReplyWithRequiredObservation(reply ownerReply) ownerReply {
	if reply.Action == ownerEnsureOther {
		reply.Context = LaunchContextReference{SHA256: ownerWireDigest}
	}
	if reply.Code == ownerOK && ownPreparationAction(reply.Action) {
		reply.Disposition = Disposition{Kind: DispositionWarmReuse}
	}
	return reply
}
