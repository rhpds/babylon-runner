package httputil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const BearerPrefix = "Bearer "

// buildJSONRequest creates an HTTP request with an optional JSON-encoded body
// and applies the supplied headers. If body is non-nil it is marshaled to JSON
// and the Content-Type header is set automatically.
func buildJSONRequest(ctx context.Context, method, url string, headers map[string]string, body interface{}) (*http.Request, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// decodeJSONResponse reads the response body and unmarshals it into result.
// When result is nil or the body is empty the response body is drained and
// discarded.
func decodeJSONResponse(resp *http.Response, result interface{}) error {
	if result == nil || resp.ContentLength == 0 {
		io.Copy(io.Discard, resp.Body)
		return nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	if len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, result); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}
	return nil
}

// DoJSON executes an HTTP request with optional JSON body and response
// decoding. body can be nil (no request body). result can be nil (response
// body is discarded). Returns the HTTP status code. Non-2xx status codes
// are NOT treated as errors — the caller decides what status codes are
// acceptable. An error is returned only for transport/marshaling failures.
func DoJSON(ctx context.Context, client *http.Client, method, url string,
	headers map[string]string, body, result interface{}) (int, error) {

	req, err := buildJSONRequest(ctx, method, url, headers, body)
	if err != nil {
		return 0, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if err := decodeJSONResponse(resp, result); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}
