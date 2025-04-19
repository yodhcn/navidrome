package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	mcp "github.com/metoro-io/mcp-golang"
	"github.com/metoro-io/mcp-golang/transport/stdio"
	"github.com/navidrome/navidrome/core/agents" // Needed for ErrNotFound
	"github.com/navidrome/navidrome/log"
	"github.com/tetratelabs/wazero"     // Needed for types
	"github.com/tetratelabs/wazero/api" // Needed for types
)

// MCPWasm implements the mcpImplementation interface for running the MCP server as a WASM module.
type MCPWasm struct {
	mu           sync.Mutex
	wasmModule   api.Module // Stores the instantiated module
	wasmCompiled api.Closer // Stores the compiled module reference for this instance
	stdin        io.WriteCloser
	client       mcpClient

	// Shared resources (passed in, not owned by this struct)
	wasmRuntime       api.Closer              // Shared Wazero Runtime
	wasmCache         wazero.CompilationCache // Shared Compilation Cache (can be nil)
	preCompiledModule wazero.CompiledModule   // Pre-compiled module from constructor

	// ClientOverride allows injecting a mock client for testing this specific implementation.
	ClientOverride mcpClient // TODO: Consider if this is the best way to test
}

// newMCPWasm creates a new instance of the WASM MCP agent implementation.
// It stores the shared runtime, cache, and the pre-compiled module.
func newMCPWasm(runtime api.Closer, cache wazero.CompilationCache, compiledModule wazero.CompiledModule) *MCPWasm {
	return &MCPWasm{
		wasmRuntime:       runtime,
		wasmCache:         cache,
		preCompiledModule: compiledModule,
	}
}

// --- mcpImplementation interface methods ---

// Close cleans up instance-specific WASM resources.
// It does NOT close the shared runtime or cache.
func (w *MCPWasm) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cleanupResources_locked()
	return nil
}

// --- Internal Helper Methods ---

// ensureClientInitialized starts the MCP WASM module and initializes the client if needed.
// MUST be called with the mutex HELD.
func (w *MCPWasm) ensureClientInitialized_locked(ctx context.Context) error {
	// Use override if provided (for testing)
	if w.ClientOverride != nil {
		if w.client == nil {
			w.client = w.ClientOverride
			log.Debug(ctx, "Using provided MCP client override for WASM testing")
		}
		return nil
	}

	// Check if already initialized
	if w.client != nil {
		return nil
	}

	log.Info(ctx, "Initializing WASM MCP client and starting/restarting server module...", "serverPath", McpServerPath)

	w.cleanupResources_locked()

	// Check if shared runtime exists
	if w.wasmRuntime == nil {
		return errors.New("shared Wazero runtime not initialized for MCPWasm")
	}

	hostStdinWriter, hostStdoutReader, mod, err := w.startModule_locked(ctx)
	if err != nil {
		log.Error(ctx, "Failed to start WASM MCP server module", "error", err)
		// Ensure pipes are closed if start failed
		if hostStdinWriter != nil {
			_ = hostStdinWriter.Close()
		}
		if hostStdoutReader != nil {
			_ = hostStdoutReader.Close()
		}
		// startModule_locked handles cleanup of mod/compiled on error
		return fmt.Errorf("failed to start WASM MCP server: %w", err)
	}

	transport := stdio.NewStdioServerTransportWithIO(hostStdoutReader, hostStdinWriter)
	clientImpl := mcp.NewClient(transport)

	initCtx, cancel := context.WithTimeout(context.Background(), initializationTimeout)
	defer cancel()
	if _, initErr := clientImpl.Initialize(initCtx); initErr != nil {
		err := fmt.Errorf("failed to initialize WASM MCP client: %w", initErr)
		log.Error(ctx, "WASM MCP client initialization failed", "error", err)
		// Cleanup the newly started module and close pipes
		w.wasmModule = mod   // Temporarily set so cleanup can close it
		w.wasmCompiled = nil // We don't store the compiled instance ref anymore, just the module instance
		w.cleanupResources_locked()
		if hostStdinWriter != nil {
			_ = hostStdinWriter.Close()
		}
		if hostStdoutReader != nil {
			_ = hostStdoutReader.Close()
		}
		return err
	}

	w.wasmModule = mod
	w.wasmCompiled = nil // We don't store the compiled instance ref anymore, just the module instance
	w.stdin = hostStdinWriter
	w.client = clientImpl

	log.Info(ctx, "WASM MCP client initialized successfully")
	return nil
}

// callMCPTool handles ensuring initialization and calling the MCP tool.
func (w *MCPWasm) callMCPTool(ctx context.Context, toolName string, args any) (string, error) {
	w.mu.Lock()
	err := w.ensureClientInitialized_locked(ctx)
	if err != nil {
		w.mu.Unlock()
		log.Error(ctx, "WASM MCP agent initialization/restart failed", "tool", toolName, "error", err)
		return "", fmt.Errorf("WASM MCP agent not ready: %w", err)
	}

	// Keep a reference to the client while locked
	currentClient := w.client
	// Unlock mutex *before* making the potentially blocking MCP call
	w.mu.Unlock()

	// Call the tool using the client reference
	log.Debug(ctx, "Calling WASM MCP tool", "tool", toolName, "args", args)
	response, callErr := currentClient.CallTool(ctx, toolName, args)
	if callErr != nil {
		// Handle potential pipe closures or other communication errors
		log.Error(ctx, "Failed to call WASM MCP tool", "tool", toolName, "error", callErr)
		// Check if the error indicates a broken pipe, suggesting the server died
		// The monitoring goroutine will handle cleanup, just return error here.
		if errors.Is(callErr, io.ErrClosedPipe) || strings.Contains(callErr.Error(), "broken pipe") || strings.Contains(callErr.Error(), "EOF") {
			log.Warn(ctx, "WASM MCP tool call failed, possibly due to server module exit.", "tool", toolName)
			// No need to explicitly call cleanup, monitoring goroutine handles it.
			return "", fmt.Errorf("WASM MCP agent module communication error: %w", callErr)
		}
		return "", fmt.Errorf("failed to call WASM MCP tool '%s': %w", toolName, callErr)
	}

	// Process the response (same logic as native)
	if response == nil || len(response.Content) == 0 || response.Content[0].TextContent == nil || response.Content[0].TextContent.Text == "" {
		log.Warn(ctx, "WASM MCP tool returned empty/invalid response", "tool", toolName)
		return "", agents.ErrNotFound
	}
	resultText := response.Content[0].TextContent.Text
	if strings.HasPrefix(resultText, "handler returned an error:") {
		log.Warn(ctx, "WASM MCP tool returned an error message", "tool", toolName, "mcpError", resultText)
		return "", agents.ErrNotFound // Treat MCP tool errors as "not found"
	}

	log.Debug(ctx, "Received response from WASM MCP agent", "tool", toolName, "length", len(resultText))
	return resultText, nil
}

// cleanupResources closes instance-specific WASM resources (stdin, module, compiled ref).
// It specifically avoids closing the shared runtime or cache.
// MUST be called with the mutex HELD.
func (w *MCPWasm) cleanupResources_locked() {
	log.Debug(context.Background(), "Cleaning up WASM MCP instance resources...")
	if w.stdin != nil {
		_ = w.stdin.Close()
		w.stdin = nil
	}
	// Close the module instance
	if w.wasmModule != nil {
		log.Debug(context.Background(), "Closing WASM module instance")
		ctxClose, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := w.wasmModule.Close(ctxClose); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			log.Error(context.Background(), "Failed to close WASM module instance", "error", err)
		}
		cancel()
		w.wasmModule = nil
	}
	// DO NOT close w.wasmCompiled (instance ref)
	// DO NOT close w.preCompiledModule (shared pre-compiled code)
	// DO NOT CLOSE w.wasmRuntime or w.wasmCache here!
	w.client = nil
}

// startModule loads and starts the MCP server as a WASM module.
// It now uses the pre-compiled module.
// MUST be called with the mutex HELD.
func (w *MCPWasm) startModule_locked(ctx context.Context) (hostStdinWriter io.WriteCloser, hostStdoutReader io.ReadCloser, mod api.Module, err error) {
	// Check for pre-compiled module
	if w.preCompiledModule == nil {
		return nil, nil, nil, errors.New("pre-compiled WASM module is nil")
	}

	// Create pipes for stdio redirection
	wasmStdinReader, hostStdinWriter, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("wasm stdin pipe: %w", err)
	}
	// Defer close pipes on error exit
	shouldClosePipesOnError := true
	defer func() {
		if shouldClosePipesOnError {
			if wasmStdinReader != nil {
				_ = wasmStdinReader.Close()
			}
			if hostStdinWriter != nil {
				_ = hostStdinWriter.Close()
			}
			// hostStdoutReader and wasmStdoutWriter handled below
		}
	}()

	hostStdoutReader, wasmStdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("wasm stdout pipe: %w", err)
	}
	// Defer close pipes on error exit
	defer func() {
		if shouldClosePipesOnError {
			if hostStdoutReader != nil {
				_ = hostStdoutReader.Close()
			}
			if wasmStdoutWriter != nil {
				_ = wasmStdoutWriter.Close()
			}
		}
	}()

	// Use the SHARDED runtime from the agent struct
	runtime, ok := w.wasmRuntime.(wazero.Runtime)
	if !ok || runtime == nil {
		return nil, nil, nil, errors.New("wasmRuntime is not initialized or not a wazero.Runtime")
	}

	// Prepare module configuration (host funcs/WASI already instantiated on runtime)
	config := wazero.NewModuleConfig().
		WithStdin(wasmStdinReader).
		WithStdout(wasmStdoutWriter).
		WithStderr(os.Stderr).
		WithArgs(McpServerPath).
		WithFS(os.DirFS("/")) // Keep FS access for now

	log.Info(ctx, "Instantiating pre-compiled WASM module (will run _start)...")
	var moduleInstance api.Module
	instanceErrChan := make(chan error, 1)
	go func() {
		var instantiateErr error
		// Use context.Background() for the module's main execution context
		moduleInstance, instantiateErr = runtime.InstantiateModule(context.Background(), w.preCompiledModule, config)
		instanceErrChan <- instantiateErr
	}()

	// Wait briefly for immediate instantiation errors
	select {
	case instantiateErr := <-instanceErrChan:
		if instantiateErr != nil {
			log.Error(ctx, "Failed to instantiate pre-compiled WASM module", "error", instantiateErr)
			// Pipes closed by defer
			return nil, nil, nil, fmt.Errorf("instantiate wasm module: %w", instantiateErr)
		}
		log.Warn(ctx, "WASM module instantiation returned (exited?) immediately without error.")
	case <-time.After(1 * time.Second): // Shorter wait now, instantiation should be faster
		log.Debug(ctx, "WASM module instantiation likely blocking (server running), proceeding...")
	}

	// Start a monitoring goroutine for WASM module exit/error
	go func(instanceToMonitor api.Module, errChan chan error) {
		// This blocks until the instance created by InstantiateModule exits or errors.
		instantiateErr := <-errChan

		w.mu.Lock() // Lock the specific MCPWasm instance
		log.Warn("WASM module exited/errored", "error", instantiateErr)

		// Critical: Check if the agent's current module is STILL the one we were monitoring.
		if w.wasmModule == instanceToMonitor {
			w.cleanupResources_locked() // Use the locked version
			log.Info("MCP WASM agent state cleaned up after module exit/error")
		} else {
			log.Debug("WASM module exited, but state already updated/module mismatch. No cleanup needed by this goroutine.")
			// No need to close anything here, the pre-compiled module is shared
		}
		w.mu.Unlock()
	}(moduleInstance, instanceErrChan)

	// Success: prevent deferred cleanup of pipes
	shouldClosePipesOnError = false
	return hostStdinWriter, hostStdoutReader, moduleInstance, nil
}
