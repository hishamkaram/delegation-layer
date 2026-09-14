package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// ProviderResponse is intentionally separate from the task control response:
// discovery has no task, admission, liveness, publication, supervisor, or
// filesystem fields.
type ProviderResponse struct {
	SchemaVersion int                          `json:"schema_version"`
	Providers     []commonprovider.Description `json:"providers"`
	Error         string                       `json:"error,omitempty"`
}

func runProviders(jsonOutput bool, stdout, stderr io.Writer, catalog commonprovider.Catalog) int {
	response := ProviderResponse{SchemaVersion: OutputSchemaVersion, Providers: catalog.Descriptions()}
	if jsonOutput {
		data, err := encodeProviderResponse(response)
		if err != nil {
			return writeProviderError(stderr, err)
		}
		if len(data) > MaxJSONResponseBytes {
			return writeProviderError(stderr, ErrJSONResponseTooLarge)
		}
		if err = writeAll(stdout, data); err != nil {
			return 1
		}
		return 0
	}
	if err := writeProviderHuman(stdout, response); err != nil {
		return 1
	}
	return 0
}

func encodeProviderResponse(response ProviderResponse) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(response); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeProviderHuman(w io.Writer, response ProviderResponse) error {
	var output strings.Builder
	for _, description := range response.Providers {
		fmt.Fprintf(&output, "provider=%s options=%s\n", description.ID, strings.Join(description.SupportedOptions, ","))
	}
	return writeAll(w, []byte(output.String()))
}

func writeProviderError(w io.Writer, err error) int {
	if err == nil {
		return 0
	}
	if _, writeErr := fmt.Fprintf(w, "error: %v\n", err); writeErr != nil {
		return 1
	}
	return 1
}
