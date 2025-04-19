package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

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
type MCPAgent struct {
	// ds model.DataStore // Not needed for this PoC
}

func mcpConstructor(ds model.DataStore) agents.Interface {
	// Check if the MCP server executable exists
	if _, err := os.Stat(mcpServerPath); os.IsNotExist(err) {
		log.Warn("MCP server executable not found, disabling agent", "path", mcpServerPath, "error", err)
		return nil
	}
	log.Info("MCP Agent initialized", "serverPath", mcpServerPath)
	return &MCPAgent{}
}

func (a *MCPAgent) AgentName() string {
	return mcpAgentName
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
	log.Debug(ctx, "Calling MCP agent GetArtistBiography", "id", id, "name", name, "mbid", mbid)

	// Prepare command with context for cancellation handling
	cmd := exec.CommandContext(ctx, mcpServerPath)

	// Get pipes for stdin and stdout
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Error(ctx, "Failed to get stdin pipe for MCP server", "error", err)
		return "", fmt.Errorf("failed to get stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Error(ctx, "Failed to get stdout pipe for MCP server", "error", err)
		return "", fmt.Errorf("failed to get stdout pipe: %w", err)
	}
	// Capture stderr for debugging, but don't block on it
	var stderr strings.Builder
	cmd.Stderr = &stderr

	// Start the MCP server process
	if err := cmd.Start(); err != nil {
		log.Error(ctx, "Failed to start MCP server process", "path", mcpServerPath, "error", err)
		return "", fmt.Errorf("failed to start MCP server: %w", err)
	}
	// Ensure the process is killed eventually
	defer func() {
		if err := cmd.Process.Kill(); err != nil {
			log.Error(ctx, "Failed to kill MCP server process", "pid", cmd.Process.Pid, "error", err)
		} else {
			log.Debug(ctx, "Killed MCP server process", "pid", cmd.Process.Pid)
		}
		// Log stderr if it contains anything
		if stderr.Len() > 0 {
			log.Warn(ctx, "MCP server stderr output", "pid", cmd.Process.Pid, "stderr", stderr.String())
		}
	}()

	log.Debug(ctx, "MCP server process started", "pid", cmd.Process.Pid)

	// Create and initialize the MCP client
	transport := stdio.NewStdioServerTransportWithIO(stdout, stdin)
	client := mcp.NewClient(transport)

	if _, err := client.Initialize(ctx); err != nil {
		log.Error(ctx, "Failed to initialize MCP client", "error", err)
		return "", fmt.Errorf("failed to initialize MCP client: %w", err)
	}
	log.Debug(ctx, "MCP client initialized")

	// Prepare arguments for the tool call
	args := getArtistBiographyArgs{
		ID:   id,
		Name: name,
		Mbid: mbid,
	}

	// Call the tool
	log.Debug(ctx, "Calling MCP tool", "tool", mcpToolName, "args", args)
	response, err := client.CallTool(ctx, mcpToolName, args)
	if err != nil {
		log.Error(ctx, "Failed to call MCP tool", "tool", mcpToolName, "error", err)
		return "", fmt.Errorf("failed to call MCP tool '%s': %w", mcpToolName, err)
	}

	// Process the response
	if response == nil || len(response.Content) == 0 || response.Content[0].TextContent == nil {
		log.Warn(ctx, "MCP tool returned empty or invalid response", "tool", mcpToolName)
		return "", agents.ErrNotFound // Or a more specific error?
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
