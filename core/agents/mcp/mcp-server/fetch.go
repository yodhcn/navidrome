package main

import (
	"context"
	"time"
)

// Fetcher defines an interface for making HTTP requests, abstracting
// over native net/http and WASM host functions.
type Fetcher interface {
	// Fetch performs an HTTP request.
	// Returns the status code, response body, and any error encountered.
	// Note: Implementations should aim to return the body even on non-2xx status codes
	// if the body was successfully read, allowing callers to potentially inspect it.
	Fetch(ctx context.Context, method, url string, requestBody []byte, timeout time.Duration) (statusCode int, responseBody []byte, err error)
}
