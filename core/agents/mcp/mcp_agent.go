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

const (
	mcpAgentName = "mcp"
	// Hardcoded path for PoC
	mcpServerPath = "/Users/deluan/Development/navidrome/plugins-mcp/mcp-server"
	mcpToolName   = "get_artist_biography"
)

// MCPAgent interacts with an external MCP server for metadata retrieval.
// It keeps a single instance of the server process running.
type MCPAgent struct {
	mu       sync.Mutex
	initOnce sync.Once
	initErr  error // Stores initialization error

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	client *mcp.Client
}

func mcpConstructor(ds model.DataStore) agents.Interface {
	// Check if the MCP server executable exists
	if _, err := os.Stat(mcpServerPath); os.IsNotExist(err) {
		log.Warn("MCP server executable not found, disabling agent", "path", mcpServerPath, "error", err)
		return nil
	}
	log.Info("MCP Agent created, server will be started on first request", "serverPath", mcpServerPath)
	// Initialize the struct, but don't start the process yet.
	return &MCPAgent{}
}

func (a *MCPAgent) AgentName() string {
	return mcpAgentName
}

// ensureClientInitialized starts the MCP server process and initializes the client exactly once.
func (a *MCPAgent) ensureClientInitialized(ctx context.Context) error {
	a.initOnce.Do(func() {
		log.Info(ctx, "Initializing MCP client and starting server process...", "serverPath", mcpServerPath)
		// Use background context for the command itself, so it doesn't get cancelled by the request context.
		// We manage its lifecycle independently.
		cmd := exec.CommandContext(context.Background(), mcpServerPath)

		stdin, err := cmd.StdinPipe()
		if err != nil {
			a.initErr = fmt.Errorf("failed to get stdin pipe for MCP server: %w", err)
			log.Error(ctx, "MCP init failed", "error", a.initErr)
			return
		}

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			a.initErr = fmt.Errorf("failed to get stdout pipe for MCP server: %w", err)
			_ = stdin.Close() // Clean up stdin pipe
			log.Error(ctx, "MCP init failed", "error", a.initErr)
			return
		}

		// Capture stderr for debugging
		var stderr strings.Builder
		cmd.Stderr = &stderr

		if err := cmd.Start(); err != nil {
			a.initErr = fmt.Errorf("failed to start MCP server process: %w", err)
			_ = stdin.Close()
			_ = stdout.Close()
			log.Error(ctx, "MCP init failed", "error", a.initErr)
			return
		}

		a.cmd = cmd
		a.stdin = stdin
		log.Info(ctx, "MCP server process started", "pid", cmd.Process.Pid)

		// Start a goroutine to wait for the process and log if it exits unexpectedly
		go func() {
			err := cmd.Wait()
			log.Warn("MCP server process exited", "pid", cmd.Process.Pid, "error", err, "stderr", stderr.String())
			// Maybe add logic here to attempt restart or mark the agent as unhealthy?
			// For PoC, just log it.
			// Ensure pipes are closed if the process exits
			a.mu.Lock()
			if a.stdin != nil {
				_ = a.stdin.Close()
				a.stdin = nil
			}
			// stdout is typically closed by the StdioServerTransport when done reading.
			a.client = nil // Mark client as unusable
			a.cmd = nil
			a.mu.Unlock()
		}()

		// Create and initialize the MCP client
		transport := stdio.NewStdioServerTransportWithIO(stdout, stdin)
		client := mcp.NewClient(transport)

		// Use a specific context for initialization, not the potentially short-lived request context
		initCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // Example timeout
		defer cancel()
		if _, err := client.Initialize(initCtx); err != nil {
			a.initErr = fmt.Errorf("failed to initialize MCP client: %w", err)
			log.Error(ctx, "MCP client initialization failed", "error", a.initErr)
			// Attempt to kill the process we just started
			if killErr := cmd.Process.Kill(); killErr != nil {
				log.Error(ctx, "Failed to kill MCP server process after init failure", "pid", cmd.Process.Pid, "error", killErr)
			}
			return
		}

		a.client = client
		log.Info(ctx, "MCP client initialized successfully")
	})
	return a.initErr
}

// getArtistBiographyArgs defines the structure for the MCP tool arguments.
// IMPORTANT: Field names MUST be exported (start with uppercase) for JSON marshalling,
// but the `json` tags determine the actual field names sent over MCP.
type getArtistBiographyArgs struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mbid string `json:"mbid,omitempty"` // Use omitempty if MBID might be absent
}

// GetArtistBiography retrieves the artist biography by calling the external MCP server.
func (a *MCPAgent) GetArtistBiography(ctx context.Context, id, name, mbid string) (string, error) {
	// Ensure the client is initialized and the server is running
	if err := a.ensureClientInitialized(ctx); err != nil {
		log.Error(ctx, "MCP client not initialized, cannot get biography", "error", err)
		// Don't wrap the initErr, return it directly so callers see the original init problem
		return "", fmt.Errorf("MCP agent not initialized: %w", err)
	}

	// Lock to ensure only one request uses the client/pipes at a time
	a.mu.Lock()
	defer a.mu.Unlock()

	// Check if the client is still valid (it might have been nilled if the process died)
	if a.client == nil {
		log.Error(ctx, "MCP client is not valid (server process likely died)")
		return "", fmt.Errorf("MCP agent process is not running")
	}

	log.Debug(ctx, "Calling MCP agent GetArtistBiography", "id", id, "name", name, "mbid", mbid)

	// Prepare arguments for the tool call
	args := getArtistBiographyArgs{
		ID:   id,
		Name: name,
		Mbid: mbid,
	}

	// Call the tool using the persistent client
	log.Debug(ctx, "Calling MCP tool", "tool", mcpToolName, "args", args)
	response, err := a.client.CallTool(ctx, mcpToolName, args)
	if err != nil {
		// Handle potential pipe closures or other communication errors
		log.Error(ctx, "Failed to call MCP tool", "tool", mcpToolName, "error", err)
		// Check if the error indicates a broken pipe, suggesting the server died
		if errors.Is(err, io.ErrClosedPipe) || strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "EOF") {
			log.Warn(ctx, "MCP tool call failed, possibly due to server process exit")
			// Attempt to reset for next time? Or just fail?
			// For now, just return a clear error.
			return "", fmt.Errorf("MCP agent process communication error: %w", err)
		}
		return "", fmt.Errorf("failed to call MCP tool '%s': %w", mcpToolName, err)
	}

	// Process the response
	if response == nil || len(response.Content) == 0 || response.Content[0].TextContent == nil {
		log.Warn(ctx, "MCP tool returned empty or invalid response", "tool", mcpToolName)
		return "", agents.ErrNotFound
	}

	bio := response.Content[0].TextContent.Text
	log.Debug(ctx, "Received biography from MCP agent", "tool", mcpToolName, "bioLength", len(bio))

	// Return the biography text
	return bio, nil
}

// Ensure MCPAgent implements the required interface
var _ agents.ArtistBiographyRetriever = (*MCPAgent)(nil)

func init() {
	agents.Register(mcpAgentName, mcpConstructor)
	log.Info("Registered MCP Agent")
}
