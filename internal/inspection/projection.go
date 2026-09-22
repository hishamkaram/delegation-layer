package inspection

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const maxProjectedFacts = 4 << 10

var (
	// ErrProjectionDefinition is returned when the immutable inspection
	// description cannot be validated before invoking a callback.
	ErrProjectionDefinition = errors.New("invalid inspection projection definition")
	// ErrProjectionDeadline rejects an unbounded caller context. The caller
	// owns the deadline; this package never installs a second timer.
	ErrProjectionDeadline = errors.New("inspection projection requires a finite deadline")
	// ErrProjectionNativeTooLarge reports native bytes outside the compiled
	// inspection output bound.
	ErrProjectionNativeTooLarge = errors.New("inspection native output exceeds its bound")
	// ErrProjectionCallback is the fixed result for callback errors and panics.
	ErrProjectionCallback = errors.New("inspection projection callback failed")
	// ErrProjectionFacts is the fixed result for malformed or oversized facts.
	ErrProjectionFacts = errors.New("inspection projection facts are invalid")
	// ErrProjectionHTTP is the fixed result for remote request, response, and
	// response-body failures. No underlying error text crosses this boundary.
	ErrProjectionHTTP = errors.New("inspection projection remote request failed")
	// ErrProjectionResponseTooLarge distinguishes the bounded-body failure
	// without exposing response bytes or transport diagnostics.
	ErrProjectionResponseTooLarge = errors.New("inspection projection response exceeds its bound")
	// ErrProjectionCanceled preserves only the standard context outcome when a
	// caller deadline or cancellation ends a remote operation.
	ErrProjectionCanceled = errors.New("inspection projection canceled")
)

// runProjection invokes one validated pure projector. A native-only
// definition invokes Project once. A remote definition invokes Authorization
// once and performs at most one HTTPS GET when required is true. The caller's
// native slice is never mutated or cleared.
func runProjection(ctx context.Context, definition provider.InspectionDefinition, native []byte) (json.RawMessage, error) {
	return runProjectionWithTransport(ctx, definition, native, nil)
}

// runProjectionWithTransport is the deterministic transport seam used by
// package tests. A nil transport selects the production transport, which has
// no proxy and leaves TLS verification to the standard library defaults.
func runProjectionWithTransport(ctx context.Context, definition provider.InspectionDefinition, native []byte, transport http.RoundTripper) (json.RawMessage, error) {
	if err := requireProjectionContext(ctx); err != nil {
		return nil, err
	}
	snapshot, _, snapshotErr := definition.Snapshot()
	if snapshotErr != nil {
		return nil, ErrProjectionDefinition
	}
	if int64(len(native)) > snapshot.OutputLimit {
		return nil, ErrProjectionNativeTooLarge
	}
	ownedNative := append([]byte(nil), native...)
	defer clear(ownedNative)

	if snapshot.Remote == nil {
		if ctxErr := projectionContextError(ctx); ctxErr != nil {
			return nil, ctxErr
		}
		facts, callbackErr := invokeNativeProject(snapshot.Project, ownedNative)
		if callbackErr != nil {
			return nil, callbackErr
		}
		if ctxErr := projectionContextError(ctx); ctxErr != nil {
			return nil, ctxErr
		}
		return canonicalProjectionFacts(facts)
	}

	return runRemoteProjection(ctx, snapshot.Remote, ownedNative, transport)
}

func runRemoteProjection(ctx context.Context, definition *provider.HTTPInspectionDefinition, native []byte, transport http.RoundTripper) (facts json.RawMessage, resultErr error) {
	if definition == nil || definition.Authorization == nil || definition.Project == nil {
		return nil, ErrProjectionDefinition
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			facts = nil
			resultErr = ErrProjectionHTTP
		}
	}()
	if ctxErr := projectionContextError(ctx); ctxErr != nil {
		return nil, ctxErr
	}

	authorization, required, callbackErr := invokeAuthorization(definition.Authorization, native)
	if callbackErr != nil {
		return nil, callbackErr
	}
	if ctxErr := projectionContextError(ctx); ctxErr != nil {
		return nil, ctxErr
	}
	if !required {
		return projectWithoutRemoteFetch(ctx, definition.Project, authorization, native)
	}
	if err := validateAuthorizationValue(authorization); err != nil {
		return nil, err
	}
	body, status, fetchErr := fetchProjectionResponse(ctx, definition, authorization, transport)
	if fetchErr != nil {
		return nil, fetchErr
	}
	defer clear(body)
	facts, callbackErr = invokeRemoteProject(definition.Project, native, status, body)
	if callbackErr != nil {
		return nil, callbackErr
	}
	if ctxErr := projectionContextError(ctx); ctxErr != nil {
		return nil, ctxErr
	}
	canonical, factsErr := canonicalProjectionFacts(facts)
	return canonical, factsErr
}

func projectWithoutRemoteFetch(ctx context.Context, project func([]byte, int, []byte) (json.RawMessage, error), authorization string, native []byte) (json.RawMessage, error) {
	if authorization != "" {
		return nil, ErrProjectionCallback
	}
	facts, callbackErr := invokeRemoteProject(project, native, 0, nil)
	if callbackErr != nil {
		return nil, callbackErr
	}
	if ctxErr := projectionContextError(ctx); ctxErr != nil {
		return nil, ctxErr
	}
	return canonicalProjectionFacts(facts)
}

func fetchProjectionResponse(ctx context.Context, definition *provider.HTTPInspectionDefinition, authorization string, transport http.RoundTripper) (body []byte, status int, resultErr error) {
	var response *http.Response
	keepBody := false
	defer func() {
		if response != nil && response.Body != nil {
			if closeErr := closeProjectionBody(response.Body); closeErr != nil && resultErr == nil {
				resultErr = ErrProjectionHTTP
			}
		}
		clearProjectionResponse(response)
		if !keepBody {
			clear(body)
		}
	}()
	defer clearFixedHeaders(definition.Headers)

	request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, definition.URL, nil)
	if requestErr != nil {
		return nil, 0, ErrProjectionHTTP
	}
	for name, value := range definition.Headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("Authorization", authorization)
	defer func() {
		// Header values are immutable strings; dropping the request references
		// prevents this operation from retaining them after the GET completes.
		clearHeaderMap(request.Header)
		request.Header = nil
		request.URL = nil
		request.Body = nil
		request.GetBody = nil
		clearFixedHeaders(definition.Headers)
		authorization = ""
	}()

	client := newProjectionHTTPClient(transport)
	defer client.CloseIdleConnections()
	var doErr error
	response, doErr = doProjectionHTTP(client, request)
	if doErr != nil {
		closeProjectionResponse(response)
		if ctxErr := projectionContextError(ctx); ctxErr != nil {
			return nil, 0, ctxErr
		}
		return nil, 0, ErrProjectionHTTP
	}
	if response == nil {
		return nil, 0, ErrProjectionHTTP
	}
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest || response.Body == nil {
		closeProjectionResponse(response)
		return nil, 0, ErrProjectionHTTP
	}
	body, readErr := readProjectionResponse(ctx, response)
	if readErr != nil {
		return nil, 0, readErr
	}
	keepBody = true
	return body, response.StatusCode, nil
}

func readProjectionResponse(ctx context.Context, response *http.Response) ([]byte, error) {
	body, readErr := readProjectionBody(response.Body, provider.MaxInspectionOutput)
	closeErr := closeProjectionBody(response.Body)
	response.Body = nil
	if errors.Is(readErr, ErrProjectionResponseTooLarge) {
		clear(body)
		return nil, ErrProjectionResponseTooLarge
	}
	if readErr != nil || closeErr != nil {
		clear(body)
		if ctxErr := projectionContextError(ctx); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, ErrProjectionHTTP
	}
	if ctxErr := projectionContextError(ctx); ctxErr != nil {
		clear(body)
		return nil, ctxErr
	}
	return body, nil
}

func readProjectionBody(reader io.Reader, maximum int64) (data []byte, resultErr error) {
	if reader == nil || maximum <= 0 {
		return nil, ErrProjectionHTTP
	}
	chunk := make([]byte, 32*1024)
	defer clear(chunk)
	data = make([]byte, 0, int(maximum))
	noProgress := 0
	for {
		n, readErr := reader.Read(chunk)
		if n < 0 || n > len(chunk) {
			clear(data)
			return nil, ErrProjectionHTTP
		}
		if n > 0 {
			noProgress = 0
			if int64(len(data))+int64(n) > maximum {
				clear(data)
				return nil, ErrProjectionResponseTooLarge
			}
			data = append(data, chunk[:n]...)
		}
		if errors.Is(readErr, io.EOF) {
			return data, nil
		}
		if readErr != nil {
			clear(data)
			return nil, ErrProjectionHTTP
		}
		if n == 0 {
			noProgress++
			if noProgress >= 100 {
				clear(data)
				return nil, ErrProjectionHTTP
			}
		}
	}
}

func requireProjectionContext(ctx context.Context) error {
	if ctx == nil {
		return ErrProjectionDeadline
	}
	if deadline, ok := ctx.Deadline(); !ok || deadline.IsZero() {
		return ErrProjectionDeadline
	}
	return projectionContextError(ctx)
}

func projectionContextError(ctx context.Context) error {
	if ctx == nil {
		return ErrProjectionDeadline
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrProjectionCanceled, err)
	}
	return nil
}

func validateAuthorizationValue(value string) error {
	if value == "" || strings.TrimSpace(value) == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n") || strings.ContainsRune(value, '\x00') {
		return ErrProjectionCallback
	}
	return nil
}

func invokeNativeProject(project func([]byte) (json.RawMessage, error), native []byte) (facts json.RawMessage, resultErr error) {
	if project == nil {
		return nil, ErrProjectionDefinition
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			facts = nil
			resultErr = ErrProjectionCallback
		}
	}()
	facts, callbackErr := project(native)
	if callbackErr != nil {
		return nil, ErrProjectionCallback
	}
	return facts, nil
}

func invokeAuthorization(authorize func([]byte) (string, bool, error), native []byte) (value string, required bool, resultErr error) {
	if authorize == nil {
		return "", false, ErrProjectionDefinition
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			value = ""
			required = false
			resultErr = ErrProjectionCallback
		}
	}()
	value, required, callbackErr := authorize(native)
	if callbackErr != nil {
		return "", false, ErrProjectionCallback
	}
	return value, required, nil
}

func invokeRemoteProject(project func([]byte, int, []byte) (json.RawMessage, error), native []byte, status int, body []byte) (facts json.RawMessage, resultErr error) {
	if project == nil {
		return nil, ErrProjectionDefinition
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			facts = nil
			resultErr = ErrProjectionCallback
		}
	}()
	facts, callbackErr := project(native, status, body)
	if callbackErr != nil {
		return nil, ErrProjectionCallback
	}
	return facts, nil
}

func canonicalProjectionFacts(raw json.RawMessage) (json.RawMessage, error) {
	return canonicalFactsWithin(raw, maxProjectedFacts)
}

func canonicalFactsWithin(raw json.RawMessage, limit int) (json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > limit || !utf8.Valid(raw) {
		return nil, ErrProjectionFacts
	}
	if err := task.ValidateJSONStructure(raw); err != nil {
		return nil, ErrProjectionFacts
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, ErrProjectionFacts
	}
	canonical, err := task.MarshalCanonical(object)
	if err != nil || len(canonical) > limit {
		return nil, ErrProjectionFacts
	}
	return json.RawMessage(canonical), nil
}
