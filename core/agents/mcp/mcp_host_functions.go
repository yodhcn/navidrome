package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/navidrome/navidrome/log"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// httpClient is a shared HTTP client for host function reuse.
var httpClient = &http.Client{
	// Consider adding a default timeout
	Timeout: 30 * time.Second,
}

// registerHostFunctions defines and registers the host functions (e.g., http_fetch)
// into the provided Wazero runtime.
func registerHostFunctions(ctx context.Context, runtime wazero.Runtime) error {
	// Define and Instantiate Host Module "env"
	_, err := runtime.NewHostModuleBuilder("env"). // "env" is the conventional module name
							NewFunctionBuilder().
							WithFunc(httpFetch).  // Register our Go function
							Export("http_fetch"). // Export it with the name WASM will use
							Instantiate(ctx)
	if err != nil {
		log.Error(ctx, "Failed to instantiate 'env' host module with httpFetch", "error", err)
		return fmt.Errorf("instantiate host module 'env': %w", err)
	}
	log.Info(ctx, "Instantiated 'env' host module with http_fetch function")
	return nil
}

// httpFetch is the host function exposed to WASM.
// ... (full implementation as provided previously) ...
// Returns:
// - 0 on success (request completed, results written).
// - 1 on host-side failure (e.g., memory access error, invalid input).
func httpFetch(
	ctx context.Context, mod api.Module, // Standard Wazero host function params
	// Request details
	urlPtr, urlLen uint32,
	methodPtr, methodLen uint32,
	bodyPtr, bodyLen uint32,
	timeoutMillis uint32,
	// Result pointers
	resultStatusPtr uint32,
	resultBodyPtr uint32, resultBodyCapacity uint32, resultBodyLenPtr uint32,
	resultErrorPtr uint32, resultErrorCapacity uint32, resultErrorLenPtr uint32,
) uint32 { // Using uint32 for status code convention (0=success, 1=failure)
	mem := mod.Memory()

	// --- Read Inputs ---
	urlBytes, ok := mem.Read(urlPtr, urlLen)
	if !ok {
		log.Error(ctx, "httpFetch host error: failed to read URL from WASM memory")
		// Cannot write error back as we don't have the pointers validated yet
		return 1
	}
	url := string(urlBytes)

	methodBytes, ok := mem.Read(methodPtr, methodLen)
	if !ok {
		log.Error(ctx, "httpFetch host error: failed to read method from WASM memory", "url", url)
		return 1 // Bail out
	}
	method := string(methodBytes)
	if method == "" {
		method = "GET" // Default to GET
	}

	var reqBody io.Reader
	if bodyLen > 0 {
		bodyBytes, ok := mem.Read(bodyPtr, bodyLen)
		if !ok {
			log.Error(ctx, "httpFetch host error: failed to read body from WASM memory", "url", url, "method", method)
			return 1 // Bail out
		}
		reqBody = bytes.NewReader(bodyBytes)
	}

	timeout := time.Duration(timeoutMillis) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second // Default timeout matching httpClient
	}

	// --- Prepare and Execute Request ---
	log.Debug(ctx, "httpFetch executing request", "method", method, "url", url, "timeout", timeout)

	// Use a specific context for the request, derived from the host function's context
	// but with the specific timeout for this call.
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, url, reqBody)
	if err != nil {
		errMsg := fmt.Sprintf("failed to create request: %v", err)
		log.Error(ctx, "httpFetch host error", "url", url, "method", method, "error", errMsg)
		writeStringResult(mem, resultErrorPtr, resultErrorCapacity, resultErrorLenPtr, errMsg)
		mem.WriteUint32Le(resultStatusPtr, 0)  // Write 0 status on creation error
		mem.WriteUint32Le(resultBodyLenPtr, 0) // No body
		return 0                               // Indicate results (including error) were written
	}

	// TODO: Consider adding a User-Agent?
	// req.Header.Set("User-Agent", "Navidrome/MCP-Agent-Host")

	resp, err := httpClient.Do(req)
	if err != nil {
		// Handle client-side errors (network, DNS, timeout)
		errMsg := fmt.Sprintf("failed to execute request: %v", err)
		log.Error(ctx, "httpFetch host error", "url", url, "method", method, "error", errMsg)
		writeStringResult(mem, resultErrorPtr, resultErrorCapacity, resultErrorLenPtr, errMsg)
		mem.WriteUint32Le(resultStatusPtr, 0) // Write 0 status on transport error
		mem.WriteUint32Le(resultBodyLenPtr, 0)
		return 0 // Indicate results written
	}
	defer resp.Body.Close()

	// --- Process Response ---
	statusCode := uint32(resp.StatusCode)
	responseBodyBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		errMsg := fmt.Sprintf("failed to read response body: %v", readErr)
		log.Error(ctx, "httpFetch host error", "url", url, "method", method, "status", statusCode, "error", errMsg)
		writeStringResult(mem, resultErrorPtr, resultErrorCapacity, resultErrorLenPtr, errMsg)
		mem.WriteUint32Le(resultStatusPtr, statusCode) // Write actual status code
		mem.WriteUint32Le(resultBodyLenPtr, 0)
		return 0 // Indicate results written
	}

	// --- Write Results Back to WASM Memory ---
	log.Debug(ctx, "httpFetch writing results", "url", url, "method", method, "status", statusCode, "bodyLen", len(responseBodyBytes))

	// Write status code
	if !mem.WriteUint32Le(resultStatusPtr, statusCode) {
		log.Error(ctx, "httpFetch host error: failed to write status code to WASM memory")
		return 1 // Host error
	}

	// Write response body (checking capacity)
	if !writeBytesResult(mem, resultBodyPtr, resultBodyCapacity, resultBodyLenPtr, responseBodyBytes) {
		// If body write fails (likely due to capacity), write an error message instead.
		errMsg := fmt.Sprintf("response body size (%d) exceeds buffer capacity (%d)", len(responseBodyBytes), resultBodyCapacity)
		log.Error(ctx, "httpFetch host error", "url", url, "method", method, "status", statusCode, "error", errMsg)
		writeStringResult(mem, resultErrorPtr, resultErrorCapacity, resultErrorLenPtr, errMsg)
		mem.WriteUint32Le(resultBodyLenPtr, 0) // Ensure body length is 0 if we wrote an error
	} else {
		// Write empty error string if body write was successful
		mem.WriteUint32Le(resultErrorLenPtr, 0)
	}

	return 0 // Success
}

// Helper to write string results, respecting capacity. Returns true on success.
func writeStringResult(mem api.Memory, ptr, capacity, lenPtr uint32, result string) bool {
	bytes := []byte(result)
	return writeBytesResult(mem, ptr, capacity, lenPtr, bytes)
}

// Helper to write byte results, respecting capacity. Returns true on success.
func writeBytesResult(mem api.Memory, ptr, capacity, lenPtr uint32, result []byte) bool {
	resultLen := uint32(len(result))
	writeLen := resultLen
	if writeLen > capacity {
		log.Warn(context.Background(), "WASM host write truncated", "requested", resultLen, "capacity", capacity)
		writeLen = capacity // Truncate if too large for buffer
	}

	if writeLen > 0 {
		if !mem.Write(ptr, result[:writeLen]) {
			log.Error(context.Background(), "WASM host memory write failed", "ptr", ptr, "len", writeLen)
			return false // Memory write failed
		}
	}

	// Write the *original* length of the data (even if truncated) so the WASM side knows.
	if !mem.WriteUint32Le(lenPtr, resultLen) {
		log.Error(context.Background(), "WASM host memory length write failed", "lenPtr", lenPtr, "len", resultLen)
		return false // Memory write failed
	}
	return true
}
