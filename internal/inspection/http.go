package inspection

import (
	"crypto/tls"
	"io"
	"net/http"
)

func newProjectionHTTPClient(transport http.RoundTripper) *http.Client {
	if transport == nil {
		transport = &http.Transport{
			// A remote inspection must never inherit an ambient proxy. Leaving
			// TLSClientConfig nil retains the standard certificate verification.
			// A single request must not leave an idle transport behind or permit
			// a connection-reuse retry.
			Proxy:             nil,
			DisableKeepAlives: true,
			// An explicit empty map keeps this one-shot client on HTTP/1.1;
			// HTTP/2 stream retries are outside the inspection contract.
			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		}
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func doProjectionHTTP(client *http.Client, request *http.Request) (response *http.Response, resultErr error) {
	defer func() {
		if recover() != nil {
			response = nil
			resultErr = ErrProjectionHTTP
		}
	}()
	return client.Do(request)
}

func closeProjectionBody(body io.Closer) (resultErr error) {
	if body == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			resultErr = ErrProjectionHTTP
		}
	}()
	if err := body.Close(); err != nil {
		return ErrProjectionHTTP
	}
	return nil
}

func closeProjectionResponse(response *http.Response) {
	if response == nil {
		return
	}
	if response.Body != nil {
		if closeErr := closeProjectionBody(response.Body); closeErr != nil {
			clearProjectionResponse(response)
			return
		}
	}
	clearProjectionResponse(response)
}

func clearProjectionResponse(response *http.Response) {
	if response == nil {
		return
	}
	clearHeaderMap(response.Header)
	clearHeaderMap(response.Trailer)
	response.Body = nil
	response.Header = nil
	response.Trailer = nil
	response.TransferEncoding = nil
	response.Request = nil
	response.TLS = nil
}

func clearHeaderMap(headers http.Header) {
	for name := range headers {
		delete(headers, name)
	}
}

func clearFixedHeaders(headers map[string]string) {
	for name := range headers {
		delete(headers, name)
	}
}
