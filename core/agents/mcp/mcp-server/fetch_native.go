//go:build !wasm

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

type nativeFetcher struct {
	// We could hold a shared client, but creating one per request
	// with the specific timeout is simpler for this adapter.
}

// Ensure nativeFetcher implements Fetcher
var _ Fetcher = (*nativeFetcher)(nil)

// NewFetcher creates the default native HTTP fetcher.
func NewFetcher() Fetcher {
	return &nativeFetcher{}
}

func (nf *nativeFetcher) Fetch(ctx context.Context, method, urlStr string, requestBody []byte, timeout time.Duration) (statusCode int, responseBody []byte, err error) {
	// Create a client with the specific timeout for this request
	client := &http.Client{Timeout: timeout}

	var bodyReader io.Reader
	if requestBody != nil {
		bodyReader = bytes.NewReader(requestBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to create native request: %w", err)
	}

	// Set headers consistent with previous direct client usage
	req.Header.Set("Accept", "application/sparql-results+json, application/json")
	// Note: Specific User-Agent was set per call site previously, might need adjustment
	// if different user agents are desired per service.
	req.Header.Set("User-Agent", "MCPGoServerExample/0.1 (Native Client)")

	resp, err := client.Do(req)
	if err != nil {
		// Let context cancellation errors pass through
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, fmt.Errorf("native HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	statusCode = resp.StatusCode
	responseBodyBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		// Still return status code if body read fails
		return statusCode, nil, fmt.Errorf("failed to read native response body: %w", readErr)
	}
	responseBody = responseBodyBytes

	// Mimic behavior of returning body even on error status
	if statusCode < 200 || statusCode >= 300 {
		return statusCode, responseBody, fmt.Errorf("native request failed with status %d", statusCode)
	}

	return statusCode, responseBody, nil
}
