//go:build !wasm

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
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
	log.Println("[MCP] Debug: Using Native HTTP fetcher")
	return &nativeFetcher{}
}

func (nf *nativeFetcher) Fetch(ctx context.Context, method, urlStr string, requestBody []byte, timeout time.Duration) (statusCode int, responseBody []byte, err error) {
	log.Printf("[MCP] Debug: Native Fetch: Method=%s, URL=%s, Timeout=%v", method, urlStr, timeout)
	// Create a client with the specific timeout for this request
	client := &http.Client{Timeout: timeout}

	var bodyReader io.Reader
	if requestBody != nil {
		bodyReader = bytes.NewReader(requestBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		log.Printf("[MCP] Error: Native Fetch failed to create request: %v", err)
		return 0, nil, fmt.Errorf("failed to create native request: %w", err)
	}

	// Set headers consistent with previous direct client usage
	req.Header.Set("Accept", "application/sparql-results+json, application/json")
	// Note: Specific User-Agent was set per call site previously, might need adjustment
	// if different user agents are desired per service.
	req.Header.Set("User-Agent", "MCPGoServerExample/0.1 (Native Client)")

	log.Printf("[MCP] Debug: Native Fetch executing request...")
	resp, err := client.Do(req)
	if err != nil {
		// Let context cancellation errors pass through
		if ctx.Err() != nil {
			log.Printf("[MCP] Debug: Native Fetch context cancelled: %v", ctx.Err())
			return 0, nil, ctx.Err()
		}
		log.Printf("[MCP] Error: Native Fetch HTTP request failed: %v", err)
		return 0, nil, fmt.Errorf("native HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	statusCode = resp.StatusCode
	log.Printf("[MCP] Debug: Native Fetch received status code: %d", statusCode)
	responseBodyBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		// Still return status code if body read fails
		log.Printf("[MCP] Error: Native Fetch failed to read response body: %v", readErr)
		return statusCode, nil, fmt.Errorf("failed to read native response body: %w", readErr)
	}
	responseBody = responseBodyBytes
	log.Printf("[MCP] Debug: Native Fetch read %d bytes from response body", len(responseBodyBytes))

	// Mimic behavior of returning body even on error status
	if statusCode < 200 || statusCode >= 300 {
		log.Printf("[MCP] Warn: Native Fetch request failed with status %d. Body: %s", statusCode, string(responseBody))
		return statusCode, responseBody, fmt.Errorf("native request failed with status %d", statusCode)
	}

	log.Printf("[MCP] Debug: Native Fetch completed successfully.")
	return statusCode, responseBody, nil
}
