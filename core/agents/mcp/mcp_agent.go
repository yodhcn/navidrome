package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	mcp "github.com/metoro-io/mcp-golang"
	"github.com/metoro-io/mcp-golang/transport/stdio"

	"github.com/navidrome/navidrome/core/agents"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

// Exported constants for testing
const (
	McpAgentName          = "mcp"
	McpServerPath         = "/Users/deluan/Development/navidrome/plugins-mcp/mcp-server"
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
type MCPAgent struct {
	mu sync.Mutex

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	client mcpClient // Use the interface type here

	// ClientOverride allows injecting a mock client for testing.
	// This field should ONLY be set in test code.
	ClientOverride mcpClient
}

func mcpConstructor(ds model.DataStore) agents.Interface {
	// Check if the MCP server executable exists
	if _, err := os.Stat(McpServerPath); os.IsNotExist(err) { // Use exported constant
		log.Warn("MCP server executable not found, disabling agent", "path", McpServerPath, "error", err)
		return nil
	}
	log.Info("MCP Agent created, server will be started on first request", "serverPath", McpServerPath)
	return &MCPAgent{}
}

func (a *MCPAgent) AgentName() string {
	return McpAgentName // Use exported constant
}

// ensureClientInitialized starts the MCP server process and initializes the client if needed.
// It now attempts restart if the client is found to be nil.
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
	a.mu.Lock() // Acquire lock *before* checking client
	if a.client != nil {
		a.mu.Unlock() // Unlock and return if already initialized
		return nil
	}

	// --- Client is nil, proceed with initialization *while holding the lock* ---
	// Defer unlock to ensure it's released even on errors during init.
	defer a.mu.Unlock()

	log.Info(ctx, "Initializing MCP client and starting/restarting server process...", "serverPath", McpServerPath)

	// Use background context for the command itself
	cmd := exec.CommandContext(context.Background(), McpServerPath)

	var stdin io.WriteCloser
	var stdout io.ReadCloser
	var stderr strings.Builder

	stdin, err = cmd.StdinPipe()
	if err != nil {
		err = fmt.Errorf("failed to get stdin pipe for MCP server: %w", err)
		log.Error(ctx, "MCP init/restart failed", "error", err)
		return err // defer mu.Unlock() will run
	}

	stdout, err = cmd.StdoutPipe()
	if err != nil {
		err = fmt.Errorf("failed to get stdout pipe for MCP server: %w", err)
		_ = stdin.Close() // Clean up stdin pipe
		log.Error(ctx, "MCP init/restart failed", "error", err)
		return err // defer mu.Unlock() will run
	}

	cmd.Stderr = &stderr

	if err = cmd.Start(); err != nil {
		err = fmt.Errorf("failed to start MCP server process: %w", err)
		_ = stdin.Close()
		_ = stdout.Close()
		log.Error(ctx, "MCP init/restart failed", "error", err)
		return err // defer mu.Unlock() will run
	}

	currentPid := cmd.Process.Pid
	log.Info(ctx, "MCP server process started/restarted", "pid", currentPid)

	// --- Start monitoring goroutine for *this* process ---
	go func(processCmd *exec.Cmd, processStderr *strings.Builder, processPid int) {
		waitErr := processCmd.Wait()
		a.mu.Lock()
		log.Warn("MCP server process exited", "pid", processPid, "error", waitErr, "stderr", processStderr.String())
		if a.cmd == processCmd { // Check if state belongs to the process that just exited
			if a.stdin != nil {
				_ = a.stdin.Close()
				a.stdin = nil
			}
			a.client = nil
			a.cmd = nil
			log.Info("MCP agent state cleaned up after process exit", "pid", processPid)
		} else {
			log.Debug("MCP agent process exited, but state already updated by newer process", "exitedPid", processPid)
		}
		a.mu.Unlock()
	}(cmd, &stderr, currentPid)

	// --- Initialize MCP client ---
	transport := stdio.NewStdioServerTransportWithIO(stdout, stdin)
	clientImpl := mcp.NewClient(transport)

	initCtx, cancel := context.WithTimeout(context.Background(), initializationTimeout)
	defer cancel()
	if _, err = clientImpl.Initialize(initCtx); err != nil {
		err = fmt.Errorf("failed to initialize MCP client: %w", err)
		log.Error(ctx, "MCP client initialization failed after process start", "pid", currentPid, "error", err)
		if killErr := cmd.Process.Kill(); killErr != nil {
			log.Error(ctx, "Failed to kill MCP server process after init failure", "pid", currentPid, "error", killErr)
		}
		return err // defer mu.Unlock() will run
	}

	// --- Initialization successful, update agent state (still holding lock) ---
	a.cmd = cmd
	a.stdin = stdin
	a.client = clientImpl

	log.Info(ctx, "MCP client initialized successfully", "pid", currentPid)
	// defer mu.Unlock() runs here
	return nil // Success
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
		log.Warn(ctx, "MCP tool returned empty or invalid response", "tool", toolName)
		return "", agents.ErrNotFound
	}

	// Return the text content
	resultText := response.Content[0].TextContent.Text
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
