//go:build wasm

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
	"unsafe"
)

// --- WASM Host Function Import --- (Copied from user prompt)

//go:wasmimport env http_fetch
//go:noescape
func http_fetch(
	// Request details
	urlPtr, urlLen uint32,
	methodPtr, methodLen uint32,
	bodyPtr, bodyLen uint32,
	timeoutMillis uint32,
	// Result pointers
	resultStatusPtr uint32,
	resultBodyPtr uint32, resultBodyCapacity uint32, resultBodyLenPtr uint32,
	resultErrorPtr uint32, resultErrorCapacity uint32, resultErrorLenPtr uint32,
) uint32 // 0 on success, 1 on host error

// --- Go Wrapper for Host Function --- (Copied from user prompt)

const (
	defaultResponseBodyCapacity  = 1024 * 10 // 10 KB for response body
	defaultResponseErrorCapacity = 1024      // 1 KB for error messages
)

// callHostHTTPFetch provides a Go-friendly interface to the http_fetch host function.
func callHostHTTPFetch(ctx context.Context, method, url string, requestBody []byte, timeout time.Duration) (statusCode int, responseBody []byte, err error) {
	log.Printf("[MCP] Debug: WASM Fetch (Host Call): Method=%s, URL=%s, Timeout=%v", method, url, timeout)

	// --- Prepare Input Pointers ---
	urlPtr, urlLen := stringToPtr(url)
	methodPtr, methodLen := stringToPtr(method)
	bodyPtr, bodyLen := bytesToPtr(requestBody)

	timeoutMillis := uint32(timeout.Milliseconds())
	if timeoutMillis <= 0 {
		timeoutMillis = 30000 // Default 30 seconds if 0 or negative
	}
	if timeout == 0 {
		// Handle case where context might already be cancelled
		select {
		case <-ctx.Done():
			log.Printf("[MCP] Debug: WASM Fetch context cancelled before host call: %v", ctx.Err())
			return 0, nil, ctx.Err()
		default:
		}
	}

	// --- Prepare Output Buffers and Pointers ---
	resultBodyBuffer := make([]byte, defaultResponseBodyCapacity)
	resultErrorBuffer := make([]byte, defaultResponseErrorCapacity)

	resultStatus := uint32(0)
	resultBodyLen := uint32(0)
	resultErrorLen := uint32(0)

	resultStatusPtr := &resultStatus
	resultBodyPtr, resultBodyCapacity := bytesToPtr(resultBodyBuffer)
	resultBodyLenPtr := &resultBodyLen
	resultErrorPtr, resultErrorCapacity := bytesToPtr(resultErrorBuffer)
	resultErrorLenPtr := &resultErrorLen

	// --- Call the Host Function ---
	log.Printf("[MCP] Debug: WASM Fetch calling host function http_fetch...")
	hostReturnCode := http_fetch(
		urlPtr, urlLen,
		methodPtr, methodLen,
		bodyPtr, bodyLen,
		timeoutMillis,
		uint32(uintptr(unsafe.Pointer(resultStatusPtr))),
		resultBodyPtr, resultBodyCapacity, uint32(uintptr(unsafe.Pointer(resultBodyLenPtr))),
		resultErrorPtr, resultErrorCapacity, uint32(uintptr(unsafe.Pointer(resultErrorLenPtr))),
	)
	log.Printf("[MCP] Debug: WASM Fetch host function returned code: %d", hostReturnCode)

	// --- Process Results ---
	if hostReturnCode != 0 {
		err = errors.New("host function http_fetch failed internally")
		log.Printf("[MCP] Error: WASM Fetch host function failed: %v", err)
		return 0, nil, err
	}

	statusCode = int(resultStatus)
	log.Printf("[MCP] Debug: WASM Fetch received status code from host: %d", statusCode)

	if resultErrorLen > 0 {
		actualErrorLen := min(resultErrorLen, resultErrorCapacity)
		errMsg := string(resultErrorBuffer[:actualErrorLen])
		err = errors.New(errMsg)
		log.Printf("[MCP] Error: WASM Fetch received error from host: %s", errMsg)
		return statusCode, nil, err
	}

	if resultBodyLen > 0 {
		actualBodyLen := min(resultBodyLen, resultBodyCapacity)
		responseBody = make([]byte, actualBodyLen)
		copy(responseBody, resultBodyBuffer[:actualBodyLen])
		log.Printf("[MCP] Debug: WASM Fetch received %d bytes from host body (reported size: %d)", actualBodyLen, resultBodyLen)

		if resultBodyLen > resultBodyCapacity {
			err = fmt.Errorf("response body truncated: received %d bytes, but actual size was %d", actualBodyLen, resultBodyLen)
			log.Printf("[MCP] Warn: WASM Fetch %v", err)
			return statusCode, responseBody, err // Return truncated body with error
		}
		log.Printf("[MCP] Debug: WASM Fetch completed successfully.")
		return statusCode, responseBody, nil
	}

	log.Printf("[MCP] Debug: WASM Fetch completed successfully (no body, no error).")
	return statusCode, nil, nil
}

// --- Pointer Helper Functions --- (Copied from user prompt)

func stringToPtr(s string) (ptr uint32, length uint32) {
	if len(s) == 0 {
		return 0, 0
	}
	// Use unsafe.StringData for potentially safer pointer access in modern Go
	// Needs Go 1.20+
	// return uint32(uintptr(unsafe.Pointer(unsafe.StringData(s)))), uint32(len(s))
	// Fallback to slice conversion for broader compatibility / if StringData isn't available
	buf := []byte(s)
	return bytesToPtr(buf)
}

func bytesToPtr(b []byte) (ptr uint32, length uint32) {
	if len(b) == 0 {
		return 0, 0
	}
	// Use unsafe.SliceData for potentially safer pointer access in modern Go
	// Needs Go 1.20+
	// return uint32(uintptr(unsafe.Pointer(unsafe.SliceData(b)))), uint32(len(b))
	// Fallback for broader compatibility
	return uint32(uintptr(unsafe.Pointer(&b[0]))), uint32(len(b))
}

func min(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}

// --- WASM Fetcher Implementation ---
type wasmFetcher struct{}

// Ensure wasmFetcher implements Fetcher
var _ Fetcher = (*wasmFetcher)(nil)

// NewFetcher creates the WASM host function fetcher.
func NewFetcher() Fetcher {
	log.Println("[MCP] Debug: Using WASM host fetcher")
	return &wasmFetcher{}
}

func (wf *wasmFetcher) Fetch(ctx context.Context, method, url string, requestBody []byte, timeout time.Duration) (statusCode int, responseBody []byte, err error) {
	// Directly call the wrapper which now contains logging
	return callHostHTTPFetch(ctx, method, url, requestBody, timeout)
}
