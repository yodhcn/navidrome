//go:build wasm

package main

import (
	"context"
	"errors"
	"fmt"
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
	hostReturnCode := http_fetch(
		urlPtr, urlLen,
		methodPtr, methodLen,
		bodyPtr, bodyLen,
		timeoutMillis,
		uint32(uintptr(unsafe.Pointer(resultStatusPtr))),
		resultBodyPtr, resultBodyCapacity, uint32(uintptr(unsafe.Pointer(resultBodyLenPtr))),
		resultErrorPtr, resultErrorCapacity, uint32(uintptr(unsafe.Pointer(resultErrorLenPtr))),
	)

	// --- Process Results ---
	if hostReturnCode != 0 {
		return 0, nil, errors.New("host function http_fetch failed internally")
	}

	statusCode = int(resultStatus)

	if resultErrorLen > 0 {
		actualErrorLen := min(resultErrorLen, resultErrorCapacity)
		errMsg := string(resultErrorBuffer[:actualErrorLen])
		return statusCode, nil, errors.New(errMsg)
	}

	if resultBodyLen > 0 {
		actualBodyLen := min(resultBodyLen, resultBodyCapacity)
		responseBody = make([]byte, actualBodyLen)
		copy(responseBody, resultBodyBuffer[:actualBodyLen])

		if resultBodyLen > resultBodyCapacity {
			err = fmt.Errorf("response body truncated: received %d bytes, but actual size was %d", actualBodyLen, resultBodyLen)
			return statusCode, responseBody, err
		}
		return statusCode, responseBody, nil
	}

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
	println("Using WASM host fetcher") // Add a log for confirmation
	return &wasmFetcher{}
}

func (wf *wasmFetcher) Fetch(ctx context.Context, method, url string, requestBody []byte, timeout time.Duration) (statusCode int, responseBody []byte, err error) {
	// Directly call the wrapper provided by the user
	// Note: Headers like Accept/User-Agent are assumed to be handled by the host
	// or aren't settable via this interface. If they are critical, the host
	// function definition (`http_fetch`) would need to be extended.
	return callHostHTTPFetch(ctx, method, url, requestBody, timeout)
}
