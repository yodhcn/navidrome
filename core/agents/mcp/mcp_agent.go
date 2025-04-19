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
		// Only set if not already set (could be set by a concurrent test setup)
		if a.client == nil {
			a.client = a.ClientOverride
			log.Debug(ctx, "Using provided MCP client override for testing")
		}
		a.mu.Unlock()
		return nil // Skip real initialization when override is present
	}

	a.mu.Lock()
	// If client is already initialized and valid, we're done.
	if a.client != nil {
		a.mu.Unlock()
		return nil
	}
	// Unlock after the check, as the rest of the function needs the lock.
	a.mu.Unlock()

	// --- Attempt initialization/restart ---
	log.Info(ctx, "Initializing MCP client and starting/restarting server process...", "serverPath", McpServerPath)

	// Use background context for the command itself, so it doesn't get cancelled by the request context.
	cmd := exec.CommandContext(context.Background(), McpServerPath)

	var stdin io.WriteCloser
	var stdout io.ReadCloser
	var stderr strings.Builder

	stdin, err = cmd.StdinPipe()
	if err != nil {
		err = fmt.Errorf("failed to get stdin pipe for MCP server: %w", err)
		log.Error(ctx, "MCP init/restart failed", "error", err)
		return err // Return error directly
	}

	stdout, err = cmd.StdoutPipe()
	if err != nil {
		err = fmt.Errorf("failed to get stdout pipe for MCP server: %w", err)
		_ = stdin.Close() // Clean up stdin pipe
		log.Error(ctx, "MCP init/restart failed", "error", err)
		return err
	}

	cmd.Stderr = &stderr

	if err = cmd.Start(); err != nil {
		err = fmt.Errorf("failed to start MCP server process: %w", err)
		_ = stdin.Close()
		_ = stdout.Close()
		log.Error(ctx, "MCP init/restart failed", "error", err)
		return err
	}

	currentPid := cmd.Process.Pid
	log.Info(ctx, "MCP server process started/restarted", "pid", currentPid)

	// --- Start monitoring goroutine for *this* process ---
	go func(processCmd *exec.Cmd, processStderr *strings.Builder, processPid int) {
		waitErr := processCmd.Wait()
		// Lock immediately after Wait returns to update state atomically
		a.mu.Lock()
		log.Warn("MCP server process exited", "pid", processPid, "error", waitErr, "stderr", processStderr.String())
		// Clean up state only if this is still the *current* process
		// (to avoid race condition if a quick restart happened)
		if a.cmd == processCmd {
			if a.stdin != nil {
				_ = a.stdin.Close()
				a.stdin = nil
			}
			a.client = nil // Mark client as unusable, triggering restart on next call
			a.cmd = nil
			log.Info("MCP agent state cleaned up after process exit", "pid", processPid)
		} else {
			log.Debug("MCP agent process exited, but state already updated by newer process", "exitedPid", processPid)
		}
		a.mu.Unlock()
	}(cmd, &stderr, currentPid) // Pass copies/values to the goroutine

	// --- Initialize MCP client ---
	transport := stdio.NewStdioServerTransportWithIO(stdout, stdin) // Use the pipes from this attempt
	// Create the *real* mcp.Client, which satisfies our mcpClient interface
	clientImpl := mcp.NewClient(transport)

	initCtx, cancel := context.WithTimeout(context.Background(), initializationTimeout)
	defer cancel()
	if _, err = clientImpl.Initialize(initCtx); err != nil {
		err = fmt.Errorf("failed to initialize MCP client: %w", err)
		log.Error(ctx, "MCP client initialization failed after process start", "pid", currentPid, "error", err)
		// Attempt to kill the process we just started, as client init failed
		if killErr := cmd.Process.Kill(); killErr != nil {
			log.Error(ctx, "Failed to kill MCP server process after init failure", "pid", currentPid, "error", killErr)
		}
		return err // Return the initialization error
	}

	// --- Initialization successful, update agent state ---
	a.mu.Lock() // Lock again to update agent state
	// Double-check if another goroutine initialized successfully in the meantime
	// Although unlikely with the outer lock/check, it's safer.
	if a.client != nil {
		a.mu.Unlock()
		log.Warn(ctx, "MCP client was already initialized by another routine, discarding this attempt", "pid", currentPid)
		// Kill the redundant process we started
		if killErr := cmd.Process.Kill(); killErr != nil {
			log.Error(ctx, "Failed to kill redundant MCP server process", "pid", currentPid, "error", killErr)
		}
		return nil // Return success as *a* client is available
	}

	a.cmd = cmd           // Store the successfully started command
	a.stdin = stdin       // Store its stdin
	a.client = clientImpl // Store the successfully initialized client (as interface type)
	a.mu.Unlock()

	log.Info(ctx, "MCP client initialized successfully", "pid", currentPid)
	return nil // Success
}

// GetArtistBiographyArgs defines the structure for the get_artist_biography MCP tool arguments.
// Exported for use in tests.
type GetArtistBiographyArgs struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mbid string `json:"mbid,omitempty"`
}

// GetArtistBiography retrieves the artist biography by calling the external MCP server.
func (a *MCPAgent) GetArtistBiography(ctx context.Context, id, name, mbid string) (string, error) {
	// Ensure the client is initialized and the server is running (attempts restart if needed)
	if err := a.ensureClientInitialized(ctx); err != nil {
		log.Error(ctx, "MCP agent initialization/restart failed, cannot get biography", "error", err)
		return "", fmt.Errorf("MCP agent not ready: %w", err)
	}

	// Lock to ensure only one request uses the client/pipes at a time
	a.mu.Lock()

	// Check if the client is still valid *after* ensuring initialization and acquiring lock.
	// The monitoring goroutine could have nilled it out if the process died just now.
	if a.client == nil {
		a.mu.Unlock() // Release lock before returning error
		log.Error(ctx, "MCP client became invalid after initialization check (server process likely died)")
		return "", fmt.Errorf("MCP agent process is not running")
	}

	// Keep a reference to the client while locked
	currentClient := a.client
	a.mu.Unlock() // Release lock before making the potentially blocking MCP call

	log.Debug(ctx, "Calling MCP agent GetArtistBiography", "id", id, "name", name, "mbid", mbid)

	// Prepare arguments for the tool call
	args := GetArtistBiographyArgs{
		ID:   id,
		Name: name,
		Mbid: mbid,
	}

	// Call the tool using the client reference
	log.Debug(ctx, "Calling MCP tool", "tool", McpToolNameGetBio, "args", args)
	response, err := currentClient.CallTool(ctx, McpToolNameGetBio, args) // Use currentClient
	if err != nil {
		// Handle potential pipe closures or other communication errors
		log.Error(ctx, "Failed to call MCP tool", "tool", McpToolNameGetBio, "error", err)
		// Check if the error indicates a broken pipe, suggesting the server died
		if errors.Is(err, io.ErrClosedPipe) || strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "EOF") {
			log.Warn(ctx, "MCP tool call failed, possibly due to server process exit. State will be reset.")
			// State reset is handled by the monitoring goroutine, just return error
			return "", fmt.Errorf("MCP agent process communication error: %w", err)
		}
		return "", fmt.Errorf("failed to call MCP tool '%s': %w", McpToolNameGetBio, err)
	}

	// Process the response
	if response == nil || len(response.Content) == 0 || response.Content[0].TextContent == nil || response.Content[0].TextContent.Text == "" {
		log.Warn(ctx, "MCP tool returned empty or invalid response", "tool", McpToolNameGetBio)
		return "", agents.ErrNotFound
	}

	bio := response.Content[0].TextContent.Text
	log.Debug(ctx, "Received biography from MCP agent", "tool", McpToolNameGetBio, "bioLength", len(bio))

	// Return the biography text
	return bio, nil
}

// GetArtistURLArgs defines the structure for the get_artist_url MCP tool arguments.
// Exported for use in tests.
type GetArtistURLArgs struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mbid string `json:"mbid,omitempty"`
}

// GetArtistURL retrieves the artist URL by calling the external MCP server.
func (a *MCPAgent) GetArtistURL(ctx context.Context, id, name, mbid string) (string, error) {
	// Ensure the client is initialized and the server is running (attempts restart if needed)
	if err := a.ensureClientInitialized(ctx); err != nil {
		log.Error(ctx, "MCP agent initialization/restart failed, cannot get URL", "error", err)
		return "", fmt.Errorf("MCP agent not ready: %w", err)
	}

	// Lock to ensure only one request uses the client/pipes at a time
	a.mu.Lock()

	// Check if the client is still valid *after* ensuring initialization and acquiring lock.
	if a.client == nil {
		a.mu.Unlock()
		log.Error(ctx, "MCP client became invalid after initialization check (server process likely died)")
		return "", fmt.Errorf("MCP agent process is not running")
	}

	// Keep a reference to the client while locked
	currentClient := a.client
	a.mu.Unlock() // Release lock before making the potentially blocking MCP call

	log.Debug(ctx, "Calling MCP agent GetArtistURL", "id", id, "name", name, "mbid", mbid)

	// Prepare arguments for the tool call
	args := GetArtistURLArgs{
		ID:   id,
		Name: name,
		Mbid: mbid,
	}

	// Call the tool using the client reference
	log.Debug(ctx, "Calling MCP tool", "tool", McpToolNameGetURL, "args", args)
	response, err := currentClient.CallTool(ctx, McpToolNameGetURL, args) // Use currentClient
	if err != nil {
		// Handle potential pipe closures or other communication errors
		log.Error(ctx, "Failed to call MCP tool", "tool", McpToolNameGetURL, "error", err)
		// Check if the error indicates a broken pipe, suggesting the server died
		if errors.Is(err, io.ErrClosedPipe) || strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "EOF") {
			log.Warn(ctx, "MCP tool call failed, possibly due to server process exit. State will be reset.")
			// State reset is handled by the monitoring goroutine, just return error
			return "", fmt.Errorf("MCP agent process communication error: %w", err)
		}
		return "", fmt.Errorf("failed to call MCP tool '%s': %w", McpToolNameGetURL, err)
	}

	// Process the response
	if response == nil || len(response.Content) == 0 || response.Content[0].TextContent == nil || response.Content[0].TextContent.Text == "" {
		log.Warn(ctx, "MCP tool returned empty or invalid response", "tool", McpToolNameGetURL)
		return "", agents.ErrNotFound
	}

	url := response.Content[0].TextContent.Text
	log.Debug(ctx, "Received URL from MCP agent", "tool", McpToolNameGetURL, "url", url)

	// Return the URL text
	return url, nil
}

// Ensure MCPAgent implements the required interfaces
var _ agents.ArtistBiographyRetriever = (*MCPAgent)(nil)
var _ agents.ArtistURLRetriever = (*MCPAgent)(nil)

func init() {
	agents.Register(McpAgentName, mcpConstructor)
	log.Info("Registered MCP Agent")
}
