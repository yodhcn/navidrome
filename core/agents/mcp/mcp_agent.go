package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mcp "github.com/metoro-io/mcp-golang"
	"github.com/metoro-io/mcp-golang/transport/stdio"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core/agents"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

// Exported constants for testing
const (
	McpAgentName          = "mcp"
	McpServerPath         = "/Users/deluan/Development/navidrome/plugins-mcp/mcp-server.wasm"
	McpToolNameGetBio     = "get_artist_biography"
	McpToolNameGetURL     = "get_artist_url"
	initializationTimeout = 10 * time.Second
)

// mcpClient interface matching the methods used from mcp.Client
// Allows for mocking in tests.
type mcpClient interface {
	Initialize(ctx context.Context) (*mcp.InitializeResponse, error)
	CallTool(ctx context.Context, toolName string, args any) (*mcp.ToolResponse, error)
}

// MCPAgent interacts with an external MCP server for metadata retrieval.
// It keeps a single instance of the server process running and attempts restart on failure.
// Supports both native executables and WASM modules (via Wazero).
type MCPAgent struct {
	mu sync.Mutex

	// Runtime state
	stdin      io.WriteCloser
	client     mcpClient
	cmd        *exec.Cmd  // Stores the native process command
	wasmModule api.Module // Stores the instantiated WASM module

	// Shared Wazero resources (created once)
	wasmRuntime api.Closer              // Shared Wazero Runtime (implements Close(context.Context))
	wasmCache   wazero.CompilationCache // Shared Compilation Cache (implements Close(context.Context))

	// WASM resources per instance (cleaned up by monitoring goroutine)
	wasmCompiled api.Closer // Stores the compiled WASM module for closing

	// ClientOverride allows injecting a mock client for testing.
	// This field should ONLY be set in test code.
	ClientOverride mcpClient
}

func mcpConstructor(ds model.DataStore) agents.Interface {
	// Check if the MCP server executable exists
	if _, err := os.Stat(McpServerPath); os.IsNotExist(err) {
		log.Warn("MCP server executable/WASM not found, disabling agent", "path", McpServerPath, "error", err)
		return nil
	}

	a := &MCPAgent{}

	// If it's a WASM path, pre-initialize the shared Wazero runtime and cache.
	if strings.HasSuffix(McpServerPath, ".wasm") {
		ctx := context.Background() // Use background context for setup
		cacheDir := filepath.Join(conf.Server.DataFolder, "cache", "wazero")
		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			log.Error(ctx, "Failed to create Wazero cache directory, WASM caching disabled", "path", cacheDir, "error", err)
		} else {
			cache, err := wazero.NewCompilationCacheWithDir(cacheDir)
			if err != nil {
				log.Error(ctx, "Failed to create Wazero compilation cache, WASM caching disabled", "path", cacheDir, "error", err)
			} else {
				// Store the specific cache type
				a.wasmCache = cache
				log.Info(ctx, "Wazero compilation cache enabled", "path", cacheDir)
			}
		}

		// Create runtime config, adding cache if it was created successfully
		runtimeConfig := wazero.NewRuntimeConfig()
		if a.wasmCache != nil {
			// Use the stored cache directly (already correct type)
			runtimeConfig = runtimeConfig.WithCompilationCache(a.wasmCache)
		}

		// Create the shared runtime
		runtime := wazero.NewRuntimeWithConfig(ctx, runtimeConfig)
		a.wasmRuntime = runtime // Store the runtime closer

		// Instantiate WASI onto the shared runtime. If this fails, the agent is unusable for WASM.
		if _, err := wasi_snapshot_preview1.Instantiate(ctx, runtime); err != nil {
			log.Error(ctx, "Failed to instantiate WASI on shared Wazero runtime, MCP WASM agent disabled", "error", err)
			// Close runtime and cache if WASI fails
			_ = runtime.Close(ctx)
			if a.wasmCache != nil {
				_ = a.wasmCache.Close(ctx) // Use Close(ctx)
			}
			return nil // Cannot proceed if WASI fails
		}
		log.Info(ctx, "Shared Wazero runtime and WASI initialized for MCP agent")
	}

	log.Info("MCP Agent created, server will be started on first request", "serverPath", McpServerPath)
	return a
}

func (a *MCPAgent) AgentName() string {
	return McpAgentName
}

// cleanup closes existing resources (stdin, server process/module).
// MUST be called while holding the mutex.
func (a *MCPAgent) cleanup() {
	log.Debug(context.Background(), "Cleaning up MCP agent instance resources...")
	if a.stdin != nil {
		_ = a.stdin.Close()
		a.stdin = nil
	}
	// Clean up native process if it exists
	if a.cmd != nil && a.cmd.Process != nil {
		log.Debug(context.Background(), "Killing native MCP process", "pid", a.cmd.Process.Pid)
		if err := a.cmd.Process.Kill(); err != nil {
			log.Error(context.Background(), "Failed to kill native process", "pid", a.cmd.Process.Pid, "error", err)
		}
		_ = a.cmd.Wait()
		a.cmd = nil
	}
	// Clean up WASM module instance if it exists
	if a.wasmModule != nil {
		log.Debug(context.Background(), "Closing WASM module instance")
		ctxClose, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := a.wasmModule.Close(ctxClose); err != nil {
			log.Error(context.Background(), "Failed to close WASM module instance", "error", err)
		}
		cancel()
		a.wasmModule = nil
	}
	// Clean up compiled module ref for this instance
	if a.wasmCompiled != nil {
		log.Debug(context.Background(), "Closing compiled WASM module ref")
		_ = a.wasmCompiled.Close(context.Background())
		a.wasmCompiled = nil
	}

	// DO NOT close shared wasmRuntime or wasmCache here.

	// Mark client as invalid
	a.client = nil
}

// ensureClientInitialized starts the MCP server process (native or WASM)
// and initializes the client if needed. Attempts restart on failure.
func (a *MCPAgent) ensureClientInitialized(ctx context.Context) (err error) {
	// --- Use override if provided (for testing) ---
	if a.ClientOverride != nil {
		a.mu.Lock()
		if a.client == nil {
			a.client = a.ClientOverride
			log.Debug(ctx, "Using provided MCP client override for testing")
		}
		a.mu.Unlock()
		return nil
	}

	// --- Check if already initialized (critical section) ---
	a.mu.Lock()
	if a.client != nil {
		a.mu.Unlock()
		return nil
	}

	// --- Client is nil, proceed with initialization *while holding the lock* ---
	defer a.mu.Unlock()

	log.Info(ctx, "Initializing MCP client and starting/restarting server process...", "serverPath", McpServerPath)

	// Clean up any old resources *before* starting new ones
	a.cleanup()

	var hostStdinWriter io.WriteCloser
	var hostStdoutReader io.ReadCloser
	var startErr error
	var isWasm bool

	if strings.HasSuffix(McpServerPath, ".wasm") {
		isWasm = true
		// Check if shared runtime exists (it should if constructor succeeded for WASM)
		if a.wasmRuntime == nil {
			startErr = errors.New("shared Wazero runtime not initialized")
		} else {
			var mod api.Module
			var compiled api.Closer // Store compiled module ref per instance
			hostStdinWriter, hostStdoutReader, mod, compiled, startErr = a.startWasmModule(ctx)
			if startErr == nil {
				a.wasmModule = mod
				// wasmRuntime is already set
				a.wasmCompiled = compiled // Store compiled ref for cleanup
			} else {
				// Ensure potential partial resources from startWasmModule are closed on error
				if mod != nil {
					_ = mod.Close(ctx)
				}
				if compiled != nil {
					_ = compiled.Close(ctx)
				}
				// Do not close shared runtime here
			}
		}
	} else {
		isWasm = false
		var nativeCmd *exec.Cmd
		hostStdinWriter, hostStdoutReader, nativeCmd, startErr = a.startNativeProcess(ctx)
		if startErr == nil {
			a.cmd = nativeCmd
		}
	}

	if startErr != nil {
		log.Error(ctx, "Failed to start MCP server process/module", "isWasm", isWasm, "error", startErr)
		// Ensure pipes are closed if start failed
		if hostStdinWriter != nil {
			_ = hostStdinWriter.Close()
		}
		if hostStdoutReader != nil {
			_ = hostStdoutReader.Close()
		}
		// a.cleanup() was already called, specific resources (cmd/wasmModule) are nil
		return fmt.Errorf("failed to start MCP server: %w", startErr)
	}

	// --- Initialize MCP client ---
	transport := stdio.NewStdioServerTransportWithIO(hostStdoutReader, hostStdinWriter)
	clientImpl := mcp.NewClient(transport)

	initCtx, cancel := context.WithTimeout(context.Background(), initializationTimeout)
	defer cancel()
	if _, err = clientImpl.Initialize(initCtx); err != nil {
		err = fmt.Errorf("failed to initialize MCP client: %w", err)
		log.Error(ctx, "MCP client initialization failed after process/module start", "isWasm", isWasm, "error", err)
		// Cleanup the newly started process/module and pipes as init failed
		a.cleanup()
		_ = hostStdinWriter.Close()
		_ = hostStdoutReader.Close()
		return err // defer mu.Unlock() will run
	}

	// --- Initialization successful, update agent state (still holding lock) ---
	a.stdin = hostStdinWriter // This is the pipe the agent writes to
	a.client = clientImpl
	// cmd or wasmModule/Runtime/Compiled are already set by the start helpers

	log.Info(ctx, "MCP client initialized successfully", "isWasm", isWasm)
	// defer mu.Unlock() runs here
	return nil // Success
}

// startNativeProcess starts the MCP server as a native executable.
func (a *MCPAgent) startNativeProcess(ctx context.Context) (stdin io.WriteCloser, stdout io.ReadCloser, cmd *exec.Cmd, err error) {
	log.Debug(ctx, "Starting native MCP server process", "path", McpServerPath)
	cmd = exec.CommandContext(context.Background(), McpServerPath)

	stdin, err = cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("native stdin pipe: %w", err)
	}

	stdout, err = cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, nil, fmt.Errorf("native stdout pipe: %w", err)
	}

	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err = cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, nil, nil, fmt.Errorf("native start: %w", err)
	}

	currentPid := cmd.Process.Pid
	log.Info(ctx, "Native MCP server process started", "pid", currentPid)

	// Start monitoring goroutine
	go func(processCmd *exec.Cmd, processStderr *strings.Builder, processPid int) {
		waitErr := processCmd.Wait() // Wait for the process to exit
		a.mu.Lock()
		log.Warn("Native MCP server process exited", "pid", processPid, "error", waitErr, "stderr", processStderr.String())
		// Check if the cmd matches the one we are monitoring before cleaning up
		if a.cmd == processCmd {
			a.cleanup() // Use the central cleanup function
			log.Info("MCP agent state cleaned up after native process exit", "pid", processPid)
		} else {
			log.Debug("Native MCP agent process exited, but state already updated or cmd mismatch", "exitedPid", processPid)
		}
		a.mu.Unlock()
	}(cmd, &stderr, currentPid)

	// Return the pipes connected to the process and the Cmd object
	return stdin, stdout, cmd, nil
}

// startWasmModule loads and starts the MCP server as a WASM module using the agent's shared Wazero runtime.
func (a *MCPAgent) startWasmModule(ctx context.Context) (hostStdinWriter io.WriteCloser, hostStdoutReader io.ReadCloser, mod api.Module, compiled api.Closer, err error) {
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
	hostStdoutReader, wasmStdoutWriter, err := os.Pipe()
	if err != nil {
		_ = wasmStdinReader.Close()
		_ = hostStdinWriter.Close()
		return nil, nil, nil, nil, fmt.Errorf("wasm stdout pipe: %w", err)
	}

	// Use the SHARDED runtime from the agent struct
	runtime := a.wasmRuntime.(wazero.Runtime) // Type assert to get underlying Runtime
	// WASI is already instantiated on the shared runtime

	config := wazero.NewModuleConfig().
		WithStdin(wasmStdinReader).
		WithStdout(wasmStdoutWriter).
		WithStderr(os.Stderr).
		WithArgs(McpServerPath)

	log.Debug(ctx, "Compiling WASM module (using cache if enabled)...")
	// Compile module using the shared runtime (which uses the configured cache)
	compiledModule, err := runtime.CompileModule(ctx, wasmBytes)
	if err != nil {
		_ = wasmStdinReader.Close()
		_ = hostStdinWriter.Close()
		_ = hostStdoutReader.Close()
		_ = wasmStdoutWriter.Close()
		return nil, nil, nil, nil, fmt.Errorf("compile wasm module: %w", err)
	}
	// Defer closing compiled module in case of errors later in this function.
	shouldCloseOnError := true
	defer func() {
		if shouldCloseOnError && compiledModule != nil {
			_ = compiledModule.Close(context.Background())
		}
	}()

	log.Info(ctx, "Instantiating WASM module (will run _start)...")
	var instance api.Module
	instanceErrChan := make(chan error, 1)
	go func() {
		var instantiateErr error
		instance, instantiateErr = runtime.InstantiateModule(context.Background(), compiledModule, config)
		instanceErrChan <- instantiateErr
	}()

	// Wait briefly for immediate instantiation errors
	select {
	case instantiateErr := <-instanceErrChan:
		if instantiateErr != nil {
			log.Error(ctx, "Failed to instantiate WASM module", "error", instantiateErr)
			_ = wasmStdinReader.Close()
			_ = hostStdinWriter.Close()
			_ = hostStdoutReader.Close()
			_ = wasmStdoutWriter.Close()
			// compiledModule closed by defer
			return nil, nil, nil, nil, fmt.Errorf("instantiate wasm module: %w", instantiateErr)
		}
		log.Warn(ctx, "WASM module instantiation returned (exited?) unexpectedly quickly.")
	case <-time.After(2 * time.Second):
		log.Debug(ctx, "WASM module instantiation likely blocking (server running), proceeding...")
	}

	// Start a monitoring goroutine for WASM module exit/error
	go func(modToMonitor api.Module, compiledToClose api.Closer, errChan chan error) {
		instantiateErr := <-errChan

		a.mu.Lock()
		log.Warn("WASM module exited/errored", "error", instantiateErr)
		// Check if the module currently stored in the agent is the one we were monitoring.
		// Compare module instance directly. Instance might be nil if instantiation failed.
		if a.wasmModule != nil && a.wasmModule == modToMonitor {
			a.cleanup() // This will close the module instance and compiled ref
			log.Info("MCP agent state cleaned up after WASM module exit/error")
		} else {
			log.Debug("WASM module exited, but state already updated or module mismatch")
			// Manually close the compiled module ref associated with this specific instance
			// as cleanup() won't if a.wasmModule doesn't match.
			if compiledToClose != nil {
				_ = compiledToClose.Close(context.Background())
			}
		}
		a.mu.Unlock()
	}(instance, compiledModule, instanceErrChan) // Pass necessary refs

	// Success: prevent deferred cleanup, return resources needed by caller
	shouldCloseOnError = false
	return hostStdinWriter, hostStdoutReader, instance, compiledModule, nil // Return instance and compiled module
}

// ArtistArgs defines the structure for MCP tool arguments requiring artist info.
// Exported for use in tests.
type ArtistArgs struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mbid string `json:"mbid,omitempty"`
}

// callMCPTool is a helper to perform the common steps of calling an MCP tool.
func (a *MCPAgent) callMCPTool(ctx context.Context, toolName string, args any) (string, error) {
	// Ensure the client is initialized and the server is running (attempts restart if needed)
	if err := a.ensureClientInitialized(ctx); err != nil {
		log.Error(ctx, "MCP agent initialization/restart failed, cannot call tool", "tool", toolName, "error", err)
		return "", fmt.Errorf("MCP agent not ready: %w", err)
	}

	// Lock to safely access the shared client resource
	a.mu.Lock()

	// Check if the client is valid *after* ensuring initialization and acquiring lock.
	if a.client == nil {
		a.mu.Unlock() // Release lock before returning error
		log.Error(ctx, "MCP client became invalid after initialization check (server process likely died)", "tool", toolName)
		return "", fmt.Errorf("MCP agent process is not running")
	}

	// Keep a reference to the client while locked
	currentClient := a.client
	a.mu.Unlock() // *Release lock before* making the potentially blocking MCP call

	// Call the tool using the client reference
	log.Debug(ctx, "Calling MCP tool", "tool", toolName, "args", args)
	response, err := currentClient.CallTool(ctx, toolName, args)
	if err != nil {
		// Handle potential pipe closures or other communication errors
		log.Error(ctx, "Failed to call MCP tool", "tool", toolName, "error", err)
		// Check if the error indicates a broken pipe, suggesting the server died
		if errors.Is(err, io.ErrClosedPipe) || strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "EOF") {
			log.Warn(ctx, "MCP tool call failed, possibly due to server process exit. State will be reset.", "tool", toolName)
			// State reset is handled by the monitoring goroutine, just return error
			return "", fmt.Errorf("MCP agent process communication error: %w", err)
		}
		return "", fmt.Errorf("failed to call MCP tool '%s': %w", toolName, err)
	}

	// Process the response
	if response == nil || len(response.Content) == 0 || response.Content[0].TextContent == nil || response.Content[0].TextContent.Text == "" {
		log.Warn(ctx, "MCP tool returned empty or invalid response structure", "tool", toolName)
		return "", agents.ErrNotFound
	}

	// Check if the returned text content itself indicates an error from the MCP tool
	resultText := response.Content[0].TextContent.Text
	if strings.HasPrefix(resultText, "handler returned an error:") {
		log.Warn(ctx, "MCP tool returned an error message in its response", "tool", toolName, "mcpError", resultText)
		return "", agents.ErrNotFound // Treat MCP tool errors as "not found"
	}

	// Return the successful text content
	log.Debug(ctx, "Received response from MCP agent", "tool", toolName, "length", len(resultText))
	return resultText, nil
}

// GetArtistBiography retrieves the artist biography by calling the external MCP server.
func (a *MCPAgent) GetArtistBiography(ctx context.Context, id, name, mbid string) (string, error) {
	args := ArtistArgs{
		ID:   id,
		Name: name,
		Mbid: mbid,
	}
	return a.callMCPTool(ctx, McpToolNameGetBio, args)
}

// GetArtistURL retrieves the artist URL by calling the external MCP server.
func (a *MCPAgent) GetArtistURL(ctx context.Context, id, name, mbid string) (string, error) {
	args := ArtistArgs{
		ID:   id,
		Name: name,
		Mbid: mbid,
	}
	return a.callMCPTool(ctx, McpToolNameGetURL, args)
}

// Ensure MCPAgent implements the required interfaces
var _ agents.ArtistBiographyRetriever = (*MCPAgent)(nil)
var _ agents.ArtistURLRetriever = (*MCPAgent)(nil)

func init() {
	agents.Register(McpAgentName, mcpConstructor)
}
