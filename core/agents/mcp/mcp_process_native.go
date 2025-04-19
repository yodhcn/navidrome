package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	mcp "github.com/metoro-io/mcp-golang"
	"github.com/metoro-io/mcp-golang/transport/stdio"
	"github.com/navidrome/navidrome/core/agents"
	"github.com/navidrome/navidrome/log"
)

// MCPNative implements the mcpImplementation interface for running the MCP server as a native process.
type MCPNative struct {
	mu     sync.Mutex
	cmd    *exec.Cmd // Stores the running command
	stdin  io.WriteCloser
	client mcpClient

	// ClientOverride allows injecting a mock client for testing this specific implementation.
	ClientOverride mcpClient // TODO: Consider if this is the best way to test
}

// newMCPNative creates a new instance of the native MCP agent implementation.
func newMCPNative() *MCPNative {
	return &MCPNative{}
}

// --- mcpImplementation interface methods ---

func (n *MCPNative) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cleanupResources_locked()
	return nil // Currently, cleanup doesn't return errors
}

// --- Internal Helper Methods ---

// ensureClientInitialized starts the MCP server process and initializes the client if needed.
// MUST be called with the mutex HELD.
func (n *MCPNative) ensureClientInitialized_locked(ctx context.Context) error {
	// Use override if provided (for testing)
	if n.ClientOverride != nil {
		if n.client == nil {
			n.client = n.ClientOverride
			log.Debug(ctx, "Using provided MCP client override for native testing")
		}
		return nil
	}

	// Check if already initialized
	if n.client != nil {
		return nil
	}

	log.Info(ctx, "Initializing Native MCP client and starting/restarting server process...", "serverPath", McpServerPath)

	// Clean up any old resources *before* starting new ones
	n.cleanupResources_locked()

	hostStdinWriter, hostStdoutReader, nativeCmd, startErr := n.startProcess_locked(ctx)
	if startErr != nil {
		log.Error(ctx, "Failed to start Native MCP server process", "error", startErr)
		// Ensure pipes are closed if start failed (startProcess might handle this, but be sure)
		if hostStdinWriter != nil {
			_ = hostStdinWriter.Close()
		}
		if hostStdoutReader != nil {
			_ = hostStdoutReader.Close()
		}
		return fmt.Errorf("failed to start native MCP server: %w", startErr)
	}

	// --- Initialize MCP client ---
	transport := stdio.NewStdioServerTransportWithIO(hostStdoutReader, hostStdinWriter)
	clientImpl := mcp.NewClient(transport)

	initCtx, cancel := context.WithTimeout(context.Background(), initializationTimeout)
	defer cancel()
	if _, initErr := clientImpl.Initialize(initCtx); initErr != nil {
		err := fmt.Errorf("failed to initialize native MCP client: %w", initErr)
		log.Error(ctx, "Native MCP client initialization failed", "error", err)
		// Cleanup the newly started process and close pipes
		n.cmd = nativeCmd // Temporarily set cmd so cleanup can kill it
		n.cleanupResources_locked()
		if hostStdinWriter != nil {
			_ = hostStdinWriter.Close()
		}
		if hostStdoutReader != nil {
			_ = hostStdoutReader.Close()
		}
		return err
	}

	// --- Initialization successful, update agent state ---
	n.cmd = nativeCmd
	n.stdin = hostStdinWriter // This is the pipe the agent writes to
	n.client = clientImpl

	log.Info(ctx, "Native MCP client initialized successfully", "pid", n.cmd.Process.Pid)
	return nil // Success
}

// callMCPTool handles ensuring initialization and calling the MCP tool.
func (n *MCPNative) callMCPTool(ctx context.Context, toolName string, args any) (string, error) {
	// Ensure the client is initialized and the server is running (attempts restart if needed)
	n.mu.Lock()
	err := n.ensureClientInitialized_locked(ctx)
	if err != nil {
		n.mu.Unlock()
		log.Error(ctx, "Native MCP agent initialization/restart failed", "tool", toolName, "error", err)
		return "", fmt.Errorf("native MCP agent not ready: %w", err)
	}

	// Keep a reference to the client while locked
	currentClient := n.client
	// Unlock mutex *before* making the potentially blocking MCP call
	n.mu.Unlock()

	// Call the tool using the client reference
	log.Debug(ctx, "Calling Native MCP tool", "tool", toolName, "args", args)
	response, callErr := currentClient.CallTool(ctx, toolName, args)
	if callErr != nil {
		// Handle potential pipe closures or other communication errors
		log.Error(ctx, "Failed to call Native MCP tool", "tool", toolName, "error", callErr)
		// Check if the error indicates a broken pipe, suggesting the server died
		// The monitoring goroutine will handle cleanup, just return error here.
		if errors.Is(callErr, io.ErrClosedPipe) || strings.Contains(callErr.Error(), "broken pipe") || strings.Contains(callErr.Error(), "EOF") {
			log.Warn(ctx, "Native MCP tool call failed, possibly due to server process exit.", "tool", toolName)
			// No need to explicitly call cleanup, monitoring goroutine handles it.
			return "", fmt.Errorf("native MCP agent process communication error: %w", callErr)
		}
		return "", fmt.Errorf("failed to call native MCP tool '%s': %w", toolName, callErr)
	}

	// Process the response (same logic as before)
	if response == nil || len(response.Content) == 0 || response.Content[0].TextContent == nil || response.Content[0].TextContent.Text == "" {
		log.Warn(ctx, "Native MCP tool returned empty/invalid response", "tool", toolName)
		// Treat as not found for agent interface consistency
		return "", agents.ErrNotFound // Import agents package if needed, or define locally
	}
	resultText := response.Content[0].TextContent.Text
	if strings.HasPrefix(resultText, "handler returned an error:") {
		log.Warn(ctx, "Native MCP tool returned an error message", "tool", toolName, "mcpError", resultText)
		return "", agents.ErrNotFound // Treat MCP tool errors as "not found"
	}

	log.Debug(ctx, "Received response from Native MCP agent", "tool", toolName, "length", len(resultText))
	return resultText, nil
}

// cleanupResources closes existing resources (stdin, server process).
// MUST be called with the mutex HELD.
func (n *MCPNative) cleanupResources_locked() {
	log.Debug(context.Background(), "Cleaning up Native MCP instance resources...")
	if n.stdin != nil {
		_ = n.stdin.Close()
		n.stdin = nil
	}
	if n.cmd != nil && n.cmd.Process != nil {
		pid := n.cmd.Process.Pid
		log.Debug(context.Background(), "Killing native MCP process", "pid", pid)
		// Kill the process. Ignore error if it's already done.
		if err := n.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			log.Error(context.Background(), "Failed to kill native process", "pid", pid, "error", err)
		}
		// Wait for the process to release resources. Ignore error.
		_ = n.cmd.Wait()
		n.cmd = nil
	}
	// Mark client as invalid
	n.client = nil
}

// startProcess starts the MCP server as a native executable and sets up monitoring.
// MUST be called with the mutex HELD.
func (n *MCPNative) startProcess_locked(ctx context.Context) (stdin io.WriteCloser, stdout io.ReadCloser, cmd *exec.Cmd, err error) {
	log.Debug(ctx, "Starting native MCP server process", "path", McpServerPath)
	// Use Background context for the command itself, as it should outlive the request context (ctx)
	cmd = exec.CommandContext(context.Background(), McpServerPath)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("native stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdinPipe.Close()
		return nil, nil, nil, fmt.Errorf("native stdout pipe: %w", err)
	}

	// Get stderr pipe to stream logs
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = stdinPipe.Close()
		_ = stdoutPipe.Close()
		return nil, nil, nil, fmt.Errorf("native stderr pipe: %w", err)
	}

	if err = cmd.Start(); err != nil {
		_ = stdinPipe.Close()
		_ = stdoutPipe.Close()
		// stderrPipe gets closed implicitly if cmd.Start() fails
		return nil, nil, nil, fmt.Errorf("native start: %w", err)
	}

	currentPid := cmd.Process.Pid
	currentCmd := cmd // Capture the current cmd pointer for the goroutine
	log.Info(ctx, "Native MCP server process started", "pid", currentPid)

	// Start monitoring goroutine for process exit
	go func() {
		// Start separate goroutine to stream stderr
		go func() {
			scanner := bufio.NewScanner(stderrPipe)
			for scanner.Scan() {
				log.Info("[MCP-SERVER] " + scanner.Text())
			}
			if err := scanner.Err(); err != nil {
				log.Error("Error reading MCP server stderr", "pid", currentPid, "error", err)
			}
			log.Debug("MCP server stderr pipe closed", "pid", currentPid)
		}()

		waitErr := currentCmd.Wait() // Wait for the specific process this goroutine monitors
		n.mu.Lock()
		// Stderr is now streamed, so we don't capture it here anymore.
		log.Warn("Native MCP server process exited", "pid", currentPid, "error", waitErr)

		// Critical: Check if the agent's current command is STILL the one we were monitoring.
		// If it's different, it means cleanup/restart already happened, so we shouldn't cleanup again.
		if n.cmd == currentCmd {
			n.cleanupResources_locked() // Use the locked version as we hold the lock
			log.Info("MCP Native agent state cleaned up after process exit", "pid", currentPid)
		} else {
			log.Debug("Native MCP process exited, but state already updated/cmd mismatch", "exitedPid", currentPid)
		}
		n.mu.Unlock()
	}()

	// Return the pipes connected to the process and the Cmd object
	return stdinPipe, stdoutPipe, cmd, nil
}
