package codex

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

func TestModelDiscoveryExchangeStart(t *testing.T) {
	definition := ModelDiscovery()
	if !reflect.DeepEqual(definition.Arguments, []string{"app-server"}) || definition.Source != modelDiscoverySource || definition.NewExchange == nil {
		t.Fatalf("unexpected discovery definition: %+v", definition)
	}
	exchange := definition.NewExchange()
	start, err := exchange.Start()
	if err != nil || len(start) != 1 {
		t.Fatalf("unexpected start: %q %v", start, err)
	}
	if strings.Contains(string(start[0]), "user") {
		t.Fatalf("initialize unexpectedly contains a user prompt: %s", start[0])
	}
	initial, err := decodeRPCMessage(start[0], "test initialize")
	if err != nil || initial.Method != "initialize" {
		t.Fatalf("unexpected initialize request: %+v %v", initial, err)
	}
	if !strings.Contains(string(start[0]), `"clientInfo"`) || !strings.Contains(string(start[0]), commonprovider.ModelsRevision) {
		t.Fatalf("initialize did not identify the discovery client: %s", start[0])
	}
}

func initializedModelExchange(t *testing.T) commonprovider.ModelExchange {
	t.Helper()
	exchange := ModelDiscovery().NewExchange()
	if _, err := exchange.Start(); err != nil {
		t.Fatal(err)
	}

	replies, done, err := exchange.Accept([]byte(`{"id":1,"result":{"serverInfo":{}}}`))
	if err != nil || done || len(replies) != 2 {
		t.Fatalf("unexpected initialize response: %q %v %v", replies, done, err)
	}
	if !strings.Contains(string(replies[0]), `"initialized"`) || !strings.Contains(string(replies[1]), `"model/list"`) {
		t.Fatalf("unexpected follow-up requests: %q", replies)
	}

	return exchange
}

func TestModelDiscoveryExchangePaginates(t *testing.T) {
	exchange := initializedModelExchange(t)
	if replies, done, err := exchange.Accept([]byte(`{"method":"server/status","params":{}}`)); err != nil || done || len(replies) != 0 {
		t.Fatalf("notification was not ignored: %q %v %v", replies, done, err)
	}
	pageOne := `{"id":2,"result":{"data":[{"id":"internal","model":"cli/model-high","displayName":"Model High"}],"nextCursor":"next"}}`
	replies, done, err := exchange.Accept([]byte(pageOne))
	if err != nil || done || len(replies) != 1 || !strings.Contains(string(replies[0]), `"cursor":"next"`) || !strings.Contains(string(replies[0]), `"id":3`) {
		t.Fatalf("pagination request was not emitted: %q %v %v", replies, done, err)
	}
	if _, done, err = exchange.Accept([]byte(`{"id":3,"result":{"data":[],"nextCursor":null}}`)); err != nil || !done {
		t.Fatalf("final page did not finish exchange: done=%v err=%v", done, err)
	}
	if _, _, err = exchange.Accept([]byte(`{"id":3,"result":{"data":[]}}`)); err == nil {
		t.Fatal("exchange accepted a response after terminal completion")
	}
}

func TestModelDiscoveryProjectsCLIModelAndEffortFacts(t *testing.T) {
	stdout := strings.Join([]string{
		`{"id":1,"result":{}}`,
		`{"method":"server/status","params":{}}`,
		`{"id":2,"result":{"data":[{"id":"internal-id","model":"provider/actual-slug","displayName":"Actual Model","supportedReasoningEfforts":["low",{"reasoningEffort":"high"}],"defaultReasoningEffort":"high"},{"id":"fallback-slug"}],"nextCursor":null}}`,
	}, "\n") + "\n"
	catalog, err := ModelDiscovery().Project([]byte(stdout), nil, []byte("--effort choices: low, high\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "available" || len(catalog.Models) != 2 {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	if catalog.Models[0].ID != "provider/actual-slug" || catalog.Models[0].DefaultEffort != "high" {
		t.Fatalf("Codex did not prefer the CLI model slug: %+v", catalog.Models[0])
	}
	if !reflect.DeepEqual(catalog.Models[0].Efforts, []string{"low", "high"}) || catalog.Models[1].ID != "fallback-slug" {
		t.Fatalf("unexpected model facts: %+v", catalog.Models)
	}
	if !reflect.DeepEqual(catalog.Efforts, []string{"low", "high"}) {
		t.Fatalf("unexpected harness efforts: %#v", catalog.Efforts)
	}
}

func TestModelDiscoveryClassifiesCorrelatedErrorsWithoutDiagnostics(t *testing.T) {
	blocked, err := ModelDiscovery().Project([]byte(`{"id":1,"error":{"code":401,"message":"authentication required: secret-token"}}`), nil, nil, true)
	if err != nil || blocked.Status != "blocked" || len(blocked.Models) != 0 {
		t.Fatalf("unexpected blocked catalog: %+v %v", blocked, err)
	}
	data, marshalErr := json.Marshal(blocked)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(data), "secret-token") {
		t.Fatal("correlated error diagnostic leaked into catalog")
	}
	failed, err := ModelDiscovery().Project([]byte(`{"id":1,"error":{"code":500,"message":"permission denied"}}`), nil, nil, true)
	if err != nil || failed.Status != "failed" || failed.ReasonCode != "native_protocol_error" {
		t.Fatalf("unexpected failed catalog: %+v %v", failed, err)
	}
	if _, err := parseModelOutput([]byte(`{"id":99,"result":{}}`)); err == nil {
		t.Fatal("uncorrelated Codex response was accepted")
	}
}

func TestModelDiscoveryDoesNotAdvertiseIncompletePagination(t *testing.T) {
	stdout := strings.Join([]string{
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"data":[{"model":"provider/first"}],"nextCursor":"more"}}`,
	}, "\n") + "\n"
	catalog, err := ModelDiscovery().Project([]byte(stdout), nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "partial" || catalog.Complete || catalog.ReasonCode != "pagination_incomplete" {
		t.Fatalf("unexpected incomplete catalog: %+v", catalog)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].ID != "provider/first" {
		t.Fatalf("partial model facts were not retained: %+v", catalog.Models)
	}
}

func TestModelDiscoveryRejectsResponsesAfterFinalPage(t *testing.T) {
	prefix := "{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":{\"data\":[{\"model\":\"first\"}],\"nextCursor\":null}}\n"
	for _, trailing := range []string{
		`{"id":0,"result":{"data":[{"model":"extra"}]}}`,
		`{"id":2,"result":{"data":[{"model":"extra"}]}}`,
		`{"id":0,"error":{"code":500,"message":"late error"}}`,
	} {
		if _, err := parseModelOutput([]byte(prefix + trailing + "\n")); err == nil {
			t.Fatalf("accepted response after completion: %s", trailing)
		}
	}
	if models, err := parseModelOutput([]byte(prefix + `{"method":"server/status","params":{}}` + "\n")); err != nil || len(models) != 1 {
		t.Fatalf("trailing notification rejected: %+v %v", models, err)
	}
}
