package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/app/remotecommands"
	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRemoteEnvelopeInspectorRejectsMalformedKnownFields(t *testing.T) {
	// Watch decisions require the same full envelope contract as ordinary CLI
	// decoding. A decoder of only pending/status would accept these malformed
	// authority and evidence fields and continue polling an invalid response.
	responseType := reflect.TypeFor[Response]()
	for i := 0; i < responseType.NumField(); i++ {
		field := responseType.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		t.Run(name, func(t *testing.T) {
			kind := field.Type.Kind()
			if kind == reflect.Pointer {
				kind = field.Type.Elem().Kind()
			}
			malformed, known := map[reflect.Kind]json.RawMessage{
				reflect.String: json.RawMessage(`{}`),
				reflect.Struct: json.RawMessage(`false`),
				reflect.Slice:  json.RawMessage(`false`),
				reflect.Int:    json.RawMessage(`"invalid"`),
				reflect.Uint64: json.RawMessage(`"invalid"`),
				reflect.Bool:   json.RawMessage(`"invalid"`),
			}[kind]
			if !known {
				t.Fatalf("add malformed boundary fixture for %s: %s", name, field.Type)
			}
			fields := map[string]json.RawMessage{"pending": json.RawMessage(`{"kind":"publication"}`), name: malformed}
			data, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if facts, decodeErr := (remoteEnvelopeInspector{}).Inspect(data); decodeErr == nil {
				t.Fatalf("malformed %s accepted as watch facts: %+v", name, facts)
			}
		})
	}
}

func TestRemoteEnvelopeInspectorProjectsWatchFacts(t *testing.T) {
	for _, test := range []struct {
		name, data string
		want       remotecommands.ObservationFacts
	}{
		{"pending", `{"pending":{"kind":"publication"}}`, remotecommands.ObservationFacts{Pending: true}},
		{"terminal", `{"outcome":{},"pending":{"kind":"publication"}}`, remotecommands.ObservationFacts{Outcome: true, Pending: true}},
		{"error", `{"error":"failed"}`, remotecommands.ObservationFacts{Error: true}},
		{"timeout", `{"status":"timed_out"}`, remotecommands.ObservationFacts{TimedOut: true}},
		{"interruption", `{"status":"interrupted"}`, remotecommands.ObservationFacts{Interrupted: true}},
		{"events", `{"events":[{}]}`, remotecommands.ObservationFacts{Events: true}},
		{"interaction", `{"interaction":{"closed":true,"pending_requests":[{}]}}`, remotecommands.ObservationFacts{InteractionClosed: true, InteractionPending: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			facts, err := (remoteEnvelopeInspector{}).Inspect([]byte(test.data))
			if err != nil || facts != test.want {
				t.Fatalf("facts=%+v want=%+v err=%v", facts, test.want, err)
			}
		})
	}
}

func TestRemotePinnedAdapterDoesNotInitializeMissingRoot(t *testing.T) {
	fixture := newRemoteServerFixture(t, false)
	pin, err := fixture.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "missing")
	store, openErr := (remoteLocalAdapter{deps: fixture.deps}).Open(t.Context(), root, true, &pin)
	if store != nil {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if openErr == nil {
		t.Fatal("pinned open initialized a different root")
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("pinned create changed missing root: %v", statErr)
	}
}

func TestRemoteCacheDiagnosticPreservesSuccessfulCLIExit(t *testing.T) {
	if code := remoteCLIResultCode("dispatch", remotecommands.Report{Diagnostics: []string{"observation_cache_unavailable"}}, nil); code != 0 {
		t.Fatalf("optional cache diagnostic changed exit to %d", code)
	}
}

func TestRemoteLocalAdapterUsesExistingOnlyReconciliation(t *testing.T) {
	fixture := newRemoteResourceBoundary(t)
	resources := &recordingResources{t: t, assessments: []func() resourceadmission.Assessment{cpuPressureNow}}
	fixture.deps.ResourceChecker = resources
	pin, err := fixture.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	adapter := remoteLocalAdapter{deps: fixture.deps}
	binding := &task.RemoteBinding{OriginRootID: fixture.intent.OriginRootID, IntentSHA256: fixture.digest}
	request := fixture.request(remote.Reconcile)
	response := remoteServerEnvelope(t, adapter.Admit(t.Context(), pin, request, binding, remote.Response{Version: remote.ProtocolVersion, Identity: fixture.intent.Destination, TaskRecord: "unknown"}))
	if response.TaskRecord != "unknown" {
		t.Fatalf("absent reconciliation changed creation state: %+v", response)
	}
	if checks, scopes := resources.calls(); checks != 0 || scopes != 0 {
		t.Fatalf("reconcile ran admission checks=%d scopes=%d", checks, scopes)
	}
	request.Operation = remote.Admit
	response = remoteServerEnvelope(t, adapter.Admit(t.Context(), pin, request, binding, remote.Response{Version: remote.ProtocolVersion, Identity: fixture.intent.Destination, TaskRecord: "not_created"}))
	if response.ResourceAdmission == nil || response.ResourceAdmission.Status != "pressure" {
		t.Fatalf("admission did not use the real destination gate: %+v", response)
	}
	if checks, scopes := resources.calls(); checks != 1 || scopes != 0 {
		t.Fatalf("admit checks=%d scopes=%d", checks, scopes)
	}
}

func TestRemoteControlParsingDoesNotMaskReferenceFailure(t *testing.T) {
	store := newBareInspectionStore(t)
	id := strings.Repeat("a", 32)
	answers := filepath.Join(t.TempDir(), "missing-answers.json")
	report, err := remoteOrigin(t.Context(), Arguments{Command: "reply", Root: store.Root, TaskID: remote.Handle(id), Answers: answers}, remoteFeature(Dependencies{ResourceChecker: fixtureResources}))
	if !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), answers) {
		t.Fatalf("local input failure masked the original reference failure: %v", err)
	}
	if report.TaskID != remote.Handle(id) || report.Connectivity != "not_connected" {
		t.Fatalf("control parsing bypassed original report setup: %+v", report)
	}
}
