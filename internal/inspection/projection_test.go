package inspection

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/provider"
)

func TestRunProjectionNativeCanonicalizesFactsAndOwnsNativeCopy(t *testing.T) {
	ctx := projectionContext(t)
	native := []byte("native-secret")
	definition := nativeInspectionDefinition(func(got []byte) (json.RawMessage, error) {
		if !bytes.Equal(got, native) {
			return nil, errors.New("native bytes changed")
		}
		got[0] = 'X'
		return json.RawMessage(`{"z":1,"a":[2]}`), nil
	})
	facts, err := runProjection(ctx, definition, native)
	if err != nil {
		t.Fatal(err)
	}
	if string(facts) != "{\"a\":[2],\"z\":1}\n" {
		t.Fatalf("facts were not canonicalized: %q", facts)
	}
	if string(native) != "native-secret" {
		t.Fatalf("caller native buffer was mutated: %q", native)
	}
}

func TestRunProjectionRequiresFiniteContext(t *testing.T) {
	definition := nativeInspectionDefinition(func([]byte) (json.RawMessage, error) {
		t.Fatal("projector called with unbounded context")
		return nil, nil
	})
	if _, err := runProjection(context.Background(), definition, nil); !errors.Is(err, ErrProjectionDeadline) {
		t.Fatalf("missing deadline error=%v", err)
	}
}

func TestRunProjectionSnapshotsBeforeCallbacks(t *testing.T) {
	called := false
	definition := nativeInspectionDefinition(func([]byte) (json.RawMessage, error) {
		called = true
		panic("secret callback panic")
	})
	definition.Executable = "relative"
	_, err := runProjection(projectionContext(t), definition, nil)
	if !errors.Is(err, ErrProjectionDefinition) || called {
		t.Fatalf("invalid definition reached callback: err=%v called=%v", err, called)
	}
}

func TestRunProjectionRejectsInvalidFacts(t *testing.T) {
	cases := map[string]json.RawMessage{
		"null":       json.RawMessage("null"),
		"array":      json.RawMessage("[]"),
		"duplicate":  json.RawMessage(`{"a":1,"a":2}`),
		"case alias": json.RawMessage(`{"a":1,"A":2}`),
		"invalid":    json.RawMessage("{\xff}"),
		"oversized":  json.RawMessage(`{"facts":"` + strings.Repeat("x", maxProjectedFacts) + `"}`),
	}
	for name, facts := range cases {
		t.Run(name, func(t *testing.T) {
			definition := nativeInspectionDefinition(func([]byte) (json.RawMessage, error) { return facts, nil })
			if got, err := runProjection(projectionContext(t), definition, nil); got != nil || !errors.Is(err, ErrProjectionFacts) {
				t.Fatalf("invalid facts accepted: got=%q err=%v", got, err)
			}
		})
	}
}

func TestRunProjectionRemoteNoFetchWhenAuthorizationNotRequired(t *testing.T) {
	var authCalled, projectCalled bool
	transport := &projectionRoundTripper{roundTrip: func(*http.Request) (*http.Response, error) {
		t.Fatal("remote request was made for a non-required authorization")
		return nil, nil
	}}
	definition := remoteInspectionDefinition(
		func([]byte) (string, bool, error) {
			authCalled = true
			return "", false, nil
		},
		func(native []byte, status int, body []byte) (json.RawMessage, error) {
			projectCalled = true
			if len(native) != len("native") || status != 0 || body != nil {
				return nil, errors.New("wrong no-fetch projector inputs")
			}
			return json.RawMessage(`{"fetched":false}`), nil
		},
	)
	facts, err := runProjectionWithTransport(projectionContext(t), definition, []byte("native"), transport)
	if err != nil || !authCalled || !projectCalled || transport.calls != 0 {
		t.Fatalf("no-fetch path failed: facts=%q err=%v auth=%v project=%v calls=%d", facts, err, authCalled, projectCalled, transport.calls)
	}
}

func TestRunProjectionRemoteFetchesOnceWithFixedHeadersAndClearsReferences(t *testing.T) {
	const secret = "Bearer sensitive-token"
	var capturedMethod, capturedURL, capturedAuthorization, capturedAccept string
	var capturedRequest *http.Request
	var capturedHeaders http.Header
	var capturedResponse *http.Response
	body := &projectionBody{Reader: bytes.NewReader([]byte(`{"remote":true}`))}
	transport := &projectionRoundTripper{roundTrip: func(request *http.Request) (*http.Response, error) {
		capturedRequest = request
		capturedMethod = request.Method
		capturedURL = request.URL.String()
		capturedAuthorization = request.Header.Get("Authorization")
		capturedAccept = request.Header.Get("Accept")
		capturedHeaders = request.Header.Clone()
		capturedResponse = &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"X-Response-Secret": []string{"response-secret"}},
			Body:       body,
			Request:    request,
		}
		return capturedResponse, nil
	}}
	definition := remoteInspectionDefinition(
		func(got []byte) (string, bool, error) {
			if string(got) != "native" {
				return "", false, errors.New("wrong native")
			}
			return secret, true, nil
		},
		func(_ []byte, status int, got []byte) (json.RawMessage, error) {
			if (projectionResponse{status: status, body: string(got)}) != (projectionResponse{status: http.StatusOK, body: `{"remote":true}`}) {
				return nil, errors.New("wrong response")
			}
			return json.RawMessage(`{"eligible":true}`), nil
		},
	)
	definition.Remote.Headers["Accept"] = "application/json"
	facts, err := runProjectionWithTransport(projectionContext(t), definition, []byte("native"), transport)
	if err != nil {
		t.Fatal(err)
	}
	if (projectionResult{facts: string(facts), calls: transport.calls}) != (projectionResult{facts: "{\"eligible\":true}\n", calls: 1}) {
		t.Fatalf("remote projection mismatch: facts=%q calls=%d", facts, transport.calls)
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
	if (projectionRequest{method: capturedMethod, url: capturedURL, authorization: capturedAuthorization, accept: capturedAccept}) != (projectionRequest{method: http.MethodGet, url: "https://provider.invalid/settings", authorization: secret, accept: "application/json"}) {
		t.Fatalf("request did not receive fixed GET binding: method=%q url=%q authorization=%q accept=%q", capturedMethod, capturedURL, capturedAuthorization, capturedAccept)
	}
	assertProjectionRequestReleased(t, capturedRequest, capturedHeaders)
	assertProjectionResponseReleased(t, capturedResponse)
}

func TestRunProjectionRemotePassesNotFoundToProjector(t *testing.T) {
	body := &projectionBody{Reader: bytes.NewReader([]byte(`{"cached":true}`))}
	transport := &projectionRoundTripper{roundTrip: func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: body, Request: request}, nil
	}}
	definition := remoteInspectionDefinition(
		func([]byte) (string, bool, error) { return "Bearer token", true, nil },
		func(_ []byte, status int, got []byte) (json.RawMessage, error) {
			if (projectionResponse{status: status, body: string(got)}) != (projectionResponse{status: http.StatusNotFound, body: `{"cached":true}`}) {
				return nil, errors.New("not found response was not projected")
			}
			return json.RawMessage(`{"remote":false}`), nil
		},
	)
	facts, err := runProjectionWithTransport(projectionContext(t), definition, nil, transport)
	if err != nil || string(facts) != "{\"remote\":false}\n" || !body.closed || transport.calls != 1 {
		t.Fatalf("404 was not delivered to projector: facts=%q err=%v closed=%v calls=%d", facts, err, body.closed, transport.calls)
	}
}

func TestRunProjectionRemoteRejectsRedirectWithoutFollowing(t *testing.T) {
	body := &projectionBody{Reader: bytes.NewReader([]byte("redirect-secret"))}
	transport := &projectionRoundTripper{roundTrip: func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://redirect.invalid"}},
			Body:       body,
			Request:    request,
		}, nil
	}}
	definition := remoteInspectionDefinition(
		func([]byte) (string, bool, error) { return "Bearer token", true, nil },
		func([]byte, int, []byte) (json.RawMessage, error) {
			t.Fatal("redirect response reached projector")
			return nil, nil
		},
	)
	if _, err := runProjectionWithTransport(projectionContext(t), definition, nil, transport); !errors.Is(err, ErrProjectionHTTP) || transport.calls != 1 || !body.closed {
		t.Fatalf("redirect was followed or leaked: err=%v calls=%d closed=%v", err, transport.calls, body.closed)
	}
}

func TestRunProjectionRemoteRejectsOversizedBodyAndCloses(t *testing.T) {
	body := &projectionBody{Reader: bytes.NewReader(bytes.Repeat([]byte{'x'}, int(provider.MaxInspectionOutput)+1))}
	transport := &projectionRoundTripper{roundTrip: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
	}}
	definition := remoteInspectionDefinition(
		func([]byte) (string, bool, error) { return "Bearer token", true, nil },
		func([]byte, int, []byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	)
	if _, err := runProjectionWithTransport(projectionContext(t), definition, nil, transport); !errors.Is(err, ErrProjectionResponseTooLarge) || !body.closed {
		t.Fatalf("oversized body was accepted or leaked: err=%v closed=%v", err, body.closed)
	}
}

func TestRunProjectionRemoteClosesBodyAfterReadPanic(t *testing.T) {
	body := &projectionBody{panicRead: true}
	transport := &projectionRoundTripper{roundTrip: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
	}}
	definition := remoteInspectionDefinition(
		func([]byte) (string, bool, error) { return "Bearer token", true, nil },
		func([]byte, int, []byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	)
	if _, err := runProjectionWithTransport(projectionContext(t), definition, nil, transport); !errors.Is(err, ErrProjectionHTTP) || !body.closed {
		t.Fatalf("read panic was not masked or body was not closed: err=%v closed=%v", err, body.closed)
	}
}

func TestProjectionHTTPClientDisablesAmbientProxyAndKeepAlive(t *testing.T) {
	client := newProjectionHTTPClient(nil)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected production transport type %T", client.Transport)
	}
	if transport.Proxy != nil || !transport.DisableKeepAlives || transport.TLSClientConfig != nil || transport.TLSNextProto == nil || len(transport.TLSNextProto) != 0 {
		t.Fatalf("production transport is not bounded and verified: proxy=%t keepalives=%t tls=%t h2=%d", transport.Proxy != nil, transport.DisableKeepAlives, transport.TLSClientConfig != nil, len(transport.TLSNextProto))
	}
	client.CloseIdleConnections()
}

func TestRunProjectionRemoteCancellationUsesCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(projectionContext(t))
	started := make(chan struct{})
	transport := &projectionRoundTripper{roundTrip: func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	}}
	definition := remoteInspectionDefinition(
		func([]byte) (string, bool, error) { return "Bearer token", true, nil },
		func([]byte, int, []byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	)
	result := make(chan error, 1)
	go func() {
		_, err := runProjectionWithTransport(ctx, definition, nil, transport)
		result <- err
	}()
	<-started
	cancel()
	err := <-result
	if !errors.Is(err, ErrProjectionCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation was not preserved safely: %v", err)
	}
}

func TestRunProjectionMasksSensitiveCallbackAndHTTPErrors(t *testing.T) {
	const secret = "super-secret-provider-detail"
	tests := map[string]func() error{
		"native callback error": func() error {
			definition := nativeInspectionDefinition(func([]byte) (json.RawMessage, error) { return nil, errors.New(secret) })
			_, err := runProjection(projectionContext(t), definition, nil)
			return err
		},
		"native callback panic": func() error {
			definition := nativeInspectionDefinition(func([]byte) (json.RawMessage, error) { panic(secret) })
			_, err := runProjection(projectionContext(t), definition, nil)
			return err
		},
		"authorization error": func() error {
			definition := remoteInspectionDefinition(func([]byte) (string, bool, error) { return "", false, errors.New(secret) }, func([]byte, int, []byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
			_, err := runProjectionWithTransport(projectionContext(t), definition, nil, &projectionRoundTripper{})
			return err
		},
		"authorization panic": func() error {
			definition := remoteInspectionDefinition(func([]byte) (string, bool, error) { panic(secret) }, func([]byte, int, []byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
			_, err := runProjectionWithTransport(projectionContext(t), definition, nil, &projectionRoundTripper{})
			return err
		},
		"project callback error": func() error {
			definition := nativeInspectionDefinition(func([]byte) (json.RawMessage, error) { return nil, errors.New(secret) })
			_, err := runProjection(projectionContext(t), definition, nil)
			return err
		},
		"remote body close error": func() error {
			body := &projectionBody{Reader: bytes.NewReader([]byte(`{}`)), closeErr: errors.New(secret)}
			transport := &projectionRoundTripper{roundTrip: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
			}}
			definition := remoteInspectionDefinition(func([]byte) (string, bool, error) { return "Bearer token", true, nil }, func([]byte, int, []byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
			_, err := runProjectionWithTransport(projectionContext(t), definition, nil, transport)
			return err
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := test()
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("sensitive callback detail escaped: %v", err)
			}
		})
	}
}

func TestRunProjectionRejectsCredentialHeadersAndAuthorizationInjection(t *testing.T) {
	fixed := nativeInspectionDefinition(func([]byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	fixed.Remote = &provider.HTTPInspectionDefinition{
		URL:           "https://provider.invalid/settings",
		Headers:       map[string]string{"Authorization": "literal-secret"},
		Authorization: func([]byte) (string, bool, error) { return "", false, nil },
		Project:       func([]byte, int, []byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	}
	if _, err := runProjectionWithTransport(projectionContext(t), fixed, nil, &projectionRoundTripper{}); !errors.Is(err, ErrProjectionDefinition) {
		t.Fatalf("literal credential header was accepted: %v", err)
	}
	definition := remoteInspectionDefinition(
		func([]byte) (string, bool, error) { return "Bearer secret\r\nX-Leak: yes", true, nil },
		func([]byte, int, []byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	)
	transport := &projectionRoundTripper{roundTrip: func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid authorization reached transport")
		return nil, nil
	}}
	if _, err := runProjectionWithTransport(projectionContext(t), definition, nil, transport); !errors.Is(err, ErrProjectionCallback) || transport.calls != 0 {
		t.Fatalf("header injection was accepted: err=%v calls=%d", err, transport.calls)
	}
}

func projectionContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func nativeInspectionDefinition(project func([]byte) (json.RawMessage, error)) provider.InspectionDefinition {
	return provider.InspectionDefinition{
		Revision:         "fixture-v1",
		Executable:       "/private/inspection-helper",
		ExecutableSHA256: strings.Repeat("a", 64),
		Arguments:        []string{"--inspect"},
		Directory:        "/private",
		Environment:      []string{"LANG=C"},
		OutputLimit:      provider.MaxInspectionOutput,
		Project:          project,
	}
}

func remoteInspectionDefinition(authorize func([]byte) (string, bool, error), project func([]byte, int, []byte) (json.RawMessage, error)) provider.InspectionDefinition {
	return provider.InspectionDefinition{
		Revision:         "fixture-v1",
		Executable:       "/private/inspection-helper",
		ExecutableSHA256: strings.Repeat("a", 64),
		Arguments:        []string{"--inspect"},
		Directory:        "/private",
		Environment:      []string{"LANG=C"},
		OutputLimit:      provider.MaxInspectionOutput,
		Remote: &provider.HTTPInspectionDefinition{
			URL:           "https://provider.invalid/settings",
			Headers:       map[string]string{},
			Authorization: authorize,
			Project:       project,
		},
	}
}

type projectionRoundTripper struct {
	mu        sync.Mutex
	calls     int
	roundTrip func(*http.Request) (*http.Response, error)
}

type projectionRequest struct {
	method        string
	url           string
	authorization string
	accept        string
}

type projectionResult struct {
	facts string
	calls int
}

type projectionResponse struct {
	status int
	body   string
}

func assertProjectionRequestReleased(t *testing.T, request *http.Request, captured http.Header) {
	t.Helper()
	if len(captured) != 2 {
		t.Fatal("request headers were not captured")
	}
	if request == nil || request.Header != nil || request.URL != nil {
		t.Fatal("credential header references were retained")
	}
}

func (t *projectionRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	if t.roundTrip == nil {
		return nil, errors.New("unexpected projection request")
	}
	return t.roundTrip(request)
}

type projectionBody struct {
	io.Reader
	closeErr  error
	closed    bool
	panicRead bool
}

func (b *projectionBody) Read(p []byte) (int, error) {
	if b.panicRead {
		panic("secret body read panic")
	}
	return b.Reader.Read(p)
}

func (b *projectionBody) Close() error {
	b.closed = true
	return b.closeErr
}

func assertProjectionResponseReleased(t *testing.T, response *http.Response) {
	t.Helper()
	if response == nil || response.Body != nil || response.Header != nil || response.Request != nil || response.TLS != nil {
		t.Fatal("response references were retained")
	}
}
