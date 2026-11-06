// Package httpjson is the one JSON request helper behind the controller's
// small API clients (coopmux, coopmuxapi, the Jira bridge): build the request,
// set the JSON and auth headers, send it, check the status and decode the
// body. Each client keeps its own error prefix and typed errors by wrapping
// what Do returns.
package httpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrorBodyLimit caps how much of a non-2xx response body a StatusError keeps.
const ErrorBodyLimit = 1024

// Request is one call.
type Request struct {
	Method string
	URL    string
	// Header is added to the request (auth, a vendor Accept). Accept
	// defaults to application/json when Header does not set it.
	Header http.Header
	// Body, when non-nil, is marshalled to JSON and sent with
	// Content-Type: application/json.
	Body any
}

// StatusError is returned by Do for any non-2xx response.
type StatusError struct {
	Method     string
	URL        string
	StatusCode int
	// Body is the start of the response body (at most ErrorBodyLimit
	// bytes), whitespace-trimmed.
	Body string
	// Header is the response header, for Retry-After and the like.
	Header http.Header
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

// Do sends req with client. A 2xx response body is decoded into out: nil
// discards it, a *[]byte receives it raw, anything else is passed to
// json.Unmarshal. A 204 leaves out untouched. A non-2xx response returns a
// *StatusError; other failures are wrapped with the step that failed.
func Do(ctx context.Context, client *http.Client, req Request, out any) error {
	var body io.Reader
	if req.Body != nil {
		data, err := json.Marshal(req.Body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		body = bytes.NewReader(data)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	if httpReq.Header.Get("Accept") == "" {
		httpReq.Header.Set("Accept", "application/json")
	}
	if req.Body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, ErrorBodyLimit))
		return &StatusError{
			Method:     req.Method,
			URL:        req.URL,
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(b)),
			Header:     resp.Header,
		}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = data
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// Bearer returns a Header carrying "Authorization: Bearer <token>", or nil
// for an empty token.
func Bearer(token string) http.Header {
	if token == "" {
		return nil
	}
	return http.Header{"Authorization": {"Bearer " + token}}
}
