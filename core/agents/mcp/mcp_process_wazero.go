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
	wasmRuntime api.Closer              // Closer for the shared Wazero Runtime
	wasmCache   wazero.CompilationCache // Shared Compilation Cache (can be nil)

	// ClientOverride allows injecting a mock client for testing this specific implementation.
	ClientOverride mcpClient // TODO: Consider if this is the best way to test
}

// newMCPWasm creates a new instance of the WASM MCP agent implementation.
func newMCPWasm(runtime api.Closer, cache wazero.CompilationCache) *MCPWasm {
	return &MCPWasm{
		wasmRuntime: runtime,
		wasmCache:   cache,
	}
}

// --- mcpImplementation interface methods ---

func (w *MCPWasm) GetArtistBiography(ctx context.Context, id, name, mbid string) (string, error) {
	args := ArtistArgs{
		ID:   id,
		Name: name,
		Mbid: mbid,
	}
	return w.callMCPTool(ctx, McpToolNameGetBio, args)
}

func (w *MCPWasm) GetArtistURL(ctx context.Context, id, name, mbid string) (string, error) {
	args := ArtistArgs{
		ID:   id,
		Name: name,
		Mbid: mbid,
	}
	return w.callMCPTool(ctx, McpToolNameGetURL, args)
}

// Close cleans up instance-specific WASM resources.
// It does NOT close the shared runtime or cache.
func (w *MCPWasm) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cleanupResources_locked()
	return nil // Currently, cleanup doesn't return errors
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

	// Clean up any old instance resources *before* starting new ones
	w.cleanupResources_locked()

	// Check if shared runtime exists (it should if constructor succeeded)
	if w.wasmRuntime == nil {
		return errors.New("shared Wazero runtime not initialized for MCPWasm")
	}

	hostStdinWriter, hostStdoutReader, mod, compiled, startErr := w.startModule_locked(ctx)
	if startErr != nil {
		log.Error(ctx, "Failed to start WASM MCP server module", "error", startErr)
		// Ensure pipes are closed if start failed
		if hostStdinWriter != nil {
			_ = hostStdinWriter.Close()
		}
		if hostStdoutReader != nil {
			_ = hostStdoutReader.Close()
		}
		// startModule_locked handles cleanup of mod/compiled on error
		return fmt.Errorf("failed to start WASM MCP server: %w", startErr)
	}

	// --- Initialize MCP client ---
	transport := stdio.NewStdioServerTransportWithIO(hostStdoutReader, hostStdinWriter)
	clientImpl := mcp.NewClient(transport)

	initCtx, cancel := context.WithTimeout(context.Background(), initializationTimeout)
	defer cancel()
	if _, initErr := clientImpl.Initialize(initCtx); initErr != nil {
		err := fmt.Errorf("failed to initialize WASM MCP client: %w", initErr)
		log.Error(ctx, "WASM MCP client initialization failed", "error", err)
		// Cleanup the newly started module and close pipes
		w.wasmModule = mod        // Temporarily set so cleanup can close it
		w.wasmCompiled = compiled // Temporarily set so cleanup can close it
		w.cleanupResources_locked()
		if hostStdinWriter != nil {
			_ = hostStdinWriter.Close()
		}
		if hostStdoutReader != nil {
			_ = hostStdoutReader.Close()
		}
		return err
	}

	// --- Initialization successful, update agent state ---
	w.wasmModule = mod
	w.wasmCompiled = compiled
	w.stdin = hostStdinWriter // This is the pipe the agent writes to
	w.client = clientImpl

	log.Info(ctx, "WASM MCP client initialized successfully")
	return nil // Success
}

// callMCPTool handles ensuring initialization and calling the MCP tool.
func (w *MCPWasm) callMCPTool(ctx context.Context, toolName string, args any) (string, error) {
	// Ensure the client is initialized and the server is running (attempts restart if needed)
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
	// Close the compiled module reference for this instance
	if w.wasmCompiled != nil {
		log.Debug(context.Background(), "Closing compiled WASM module ref")
		if err := w.wasmCompiled.Close(context.Background()); err != nil {
			log.Error(context.Background(), "Failed to close compiled WASM module ref", "error", err)
		}
		w.wasmCompiled = nil
	}
	// Mark client as invalid
	w.client = nil
	// DO NOT CLOSE w.wasmRuntime or w.wasmCache here!
}

// startModule loads and starts the MCP server as a WASM module.
// MUST be called with the mutex HELD.
func (w *MCPWasm) startModule_locked(ctx context.Context) (hostStdinWriter io.WriteCloser, hostStdoutReader io.ReadCloser, mod api.Module, compiled api.Closer, err error) {
	log.Debug(ctx, "Loading WASM MCP server module", "path", McpServerPath)
	wasmBytes, err := os.ReadFile(McpServerPath)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read wasm file: %w", err)
	}

	// Create pipes for stdio redirection
	wasmStdinReader, hostStdinWriter, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("wasm stdin pipe: %w", err)
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
		return nil, nil, nil, nil, fmt.Errorf("wasm stdout pipe: %w", err)
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
		return nil, nil, nil, nil, errors.New("wasmRuntime is not initialized or not a wazero.Runtime")
	}

	// Prepare module configuration
	// Host functions and WASI are already part of the shared runtime
	config := wazero.NewModuleConfig().
		WithStdin(wasmStdinReader).
		WithStdout(wasmStdoutWriter).
		WithStderr(os.Stderr).
		WithArgs(McpServerPath).
		WithFS(os.DirFS("/")) // Keep FS access for now

	log.Debug(ctx, "Compiling WASM module (using cache if enabled)...")
	// Compile module using the shared runtime
	compiledModule, err := runtime.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("compile wasm module: %w", err)
	}
	// Defer closing compiled module only if an error occurs later in this function.
	shouldCloseCompiledOnError := true
	defer func() {
		if shouldCloseCompiledOnError && compiledModule != nil {
			_ = compiledModule.Close(context.Background())
		}
	}()

	log.Info(ctx, "Instantiating WASM module (will run _start)...")
	var instance api.Module
	instanceErrChan := make(chan error, 1)
	go func() {
		var instantiateErr error
		// Use context.Background() for the module's main execution context
		instance, instantiateErr = runtime.InstantiateModule(context.Background(), compiledModule, config)
		instanceErrChan <- instantiateErr
	}()

	// Wait briefly for immediate instantiation errors
	select {
	case instantiateErr := <-instanceErrChan:
		if instantiateErr != nil {
			log.Error(ctx, "Failed to instantiate WASM module", "error", instantiateErr)
			// compiledModule closed by defer
			// pipes closed by defer
			return nil, nil, nil, nil, fmt.Errorf("instantiate wasm module: %w", instantiateErr)
		}
		log.Warn(ctx, "WASM module instantiation returned (exited?) immediately without error.")
	case <-time.After(2 * time.Second):
		log.Debug(ctx, "WASM module instantiation likely blocking (server running), proceeding...")
	}

	// Start a monitoring goroutine for WASM module exit/error
	// Pass required values to the goroutine closure
	go func(instanceToMonitor api.Module, compiledToClose api.Closer, errChan chan error) {
		// This blocks until the instance created by InstantiateModule exits or errors.
		instantiateErr := <-errChan

		w.mu.Lock() // Lock the specific MCPWasm instance
		log.Warn("WASM module exited/errored", "error", instantiateErr)

		// Critical: Check if the agent's current module is STILL the one we were monitoring.
		if w.wasmModule == instanceToMonitor {
			w.cleanupResources_locked() // Use the locked version
			log.Info("MCP WASM agent state cleaned up after module exit/error")
		} else {
			log.Debug("WASM module exited, but state already updated/module mismatch. Explicitly closing this instance's compiled ref.")
			// Manually close the compiled module ref associated with *this specific instance*
			if compiledToClose != nil {
				_ = compiledToClose.Close(context.Background())
			}
		}
		w.mu.Unlock()
	}(instance, compiledModule, instanceErrChan)

	// Success: prevent deferred cleanup of pipes and compiled module
	shouldClosePipesOnError = false
	shouldCloseCompiledOnError = false
	return hostStdinWriter, hostStdoutReader, instance, compiledModule, nil
}
