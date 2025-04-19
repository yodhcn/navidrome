package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcp "github.com/metoro-io/mcp-golang"
	"github.com/tetratelabs/wazero"

	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core/agents"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

// Constants used by the MCP agent
const (
	McpAgentName          = "mcp"
	initializationTimeout = 5 * time.Second
	// McpServerPath defines the location of the MCP server executable or WASM module.
	McpServerPath     = "./core/agents/mcp/mcp-server/mcp-server.wasm"
	McpToolNameGetBio = "get_artist_biography"
	McpToolNameGetURL = "get_artist_url"
)

// mcpClient interface matching the methods used from mcp.Client.
type mcpClient interface {
	Initialize(ctx context.Context) (*mcp.InitializeResponse, error)
	CallTool(ctx context.Context, toolName string, args any) (*mcp.ToolResponse, error)
}

// mcpImplementation defines the common interface for both native and WASM MCP agents.
// This allows the main MCPAgent to delegate calls without knowing the underlying type.
type mcpImplementation interface {
	Close() error // For cleaning up resources associated with this specific implementation.

	// callMCPTool is the core method implemented differently by native/wasm
	callMCPTool(ctx context.Context, toolName string, args any) (string, error)
}

// MCPAgent is the public-facing agent registered with Navidrome.
// It acts as a wrapper around the actual implementation (native or WASM).
type MCPAgent struct {
	// No mutex needed here if impl is set once at construction
	// and the implementation handles its own internal state synchronization.
	impl mcpImplementation

	// Shared Wazero resources (runtime, cache) are managed externally
	// and closed separately, likely during application shutdown.
}

// mcpConstructor creates the appropriate MCP implementation (native or WASM)
// and wraps it in the MCPAgent.
func mcpConstructor(ds model.DataStore) agents.Interface {
	if _, err := os.Stat(McpServerPath); os.IsNotExist(err) {
		log.Warn("MCP server executable/WASM not found, disabling agent", "path", McpServerPath, "error", err)
		return nil
	}

	var agentImpl mcpImplementation
	var err error

	if strings.HasSuffix(McpServerPath, ".wasm") {
		log.Info("Configuring MCP agent for WASM execution", "path", McpServerPath)
		ctx := context.Background()

		// Setup Shared Wazero Resources
		var cache wazero.CompilationCache
		cacheDir := filepath.Join(conf.Server.DataFolder, "cache", "wazero")
		if errMkdir := os.MkdirAll(cacheDir, 0755); errMkdir != nil {
			log.Error(ctx, "Failed to create Wazero cache directory, WASM caching disabled", "path", cacheDir, "error", errMkdir)
		} else {
			cache, err = wazero.NewCompilationCacheWithDir(cacheDir)
			if err != nil {
				log.Error(ctx, "Failed to create Wazero compilation cache, WASM caching disabled", "path", cacheDir, "error", err)
				cache = nil
			}
		}

		runtimeConfig := wazero.NewRuntimeConfig()
		if cache != nil {
			runtimeConfig = runtimeConfig.WithCompilationCache(cache)
		}

		runtime := wazero.NewRuntimeWithConfig(ctx, runtimeConfig)

		if err = registerHostFunctions(ctx, runtime); err != nil {
			_ = runtime.Close(ctx)
			if cache != nil {
				_ = cache.Close(ctx)
			}
			return nil // Fatal error: Host functions required
		}

		if _, err = wasi_snapshot_preview1.Instantiate(ctx, runtime); err != nil {
			log.Error(ctx, "Failed to instantiate WASI on shared Wazero runtime, MCP WASM agent disabled", "error", err)
			_ = runtime.Close(ctx)
			if cache != nil {
				_ = cache.Close(ctx)
			}
			return nil // Fatal error: WASI required
		}

		// Compile the module
		log.Debug(ctx, "Pre-compiling WASM module...", "path", McpServerPath)
		wasmBytes, err := os.ReadFile(McpServerPath)
		if err != nil {
			log.Error(ctx, "Failed to read WASM module file, disabling agent", "path", McpServerPath, "error", err)
			_ = runtime.Close(ctx)
			if cache != nil {
				_ = cache.Close(ctx)
			}
			return nil
		}
		compiledModule, err := runtime.CompileModule(ctx, wasmBytes)
		if err != nil {
			log.Error(ctx, "Failed to pre-compile WASM module, disabling agent", "path", McpServerPath, "error", err)
			_ = runtime.Close(ctx)
			if cache != nil {
				_ = cache.Close(ctx)
			}
			return nil
		}

		agentImpl = newMCPWasm(runtime, cache, compiledModule)
		log.Info(ctx, "Shared Wazero runtime, WASI, cache, host functions initialized, and module pre-compiled for MCP agent")

	} else {
		log.Info("Configuring MCP agent for native execution", "path", McpServerPath)
		agentImpl = newMCPNative()
	}

	log.Info("MCP Agent implementation created successfully")
	return &MCPAgent{impl: agentImpl}
}

// NewAgentForTesting is a constructor specifically for tests.
// It creates the appropriate implementation based on McpServerPath
// and injects a mock mcpClient into its ClientOverride field.
func NewAgentForTesting(mockClient mcpClient) agents.Interface {
	// We need to replicate the logic from mcpConstructor to determine
	// the implementation type, but without actually starting processes.

	var agentImpl mcpImplementation

	if strings.HasSuffix(McpServerPath, ".wasm") {
		// For WASM testing, we might not need the full runtime setup,
		// just the struct. Pass nil for shared resources for now.
		// We rely on the mockClient being used before any real WASM interaction.
		wasmImpl := newMCPWasm(nil, nil, nil)
		wasmImpl.ClientOverride = mockClient
		agentImpl = wasmImpl
	} else {
		nativeImpl := newMCPNative()
		nativeImpl.ClientOverride = mockClient
		agentImpl = nativeImpl
	}

	return &MCPAgent{impl: agentImpl}
}

func (a *MCPAgent) AgentName() string {
	return McpAgentName
}

func (a *MCPAgent) GetArtistBiography(ctx context.Context, id, name, mbid string) (string, error) {
	if a.impl == nil {
		return "", errors.New("MCP agent implementation is nil")
	}
	// Construct args and call the implementation's specific tool caller
	args := ArtistArgs{ID: id, Name: name, Mbid: mbid}
	return a.impl.callMCPTool(ctx, McpToolNameGetBio, args)
}

func (a *MCPAgent) GetArtistURL(ctx context.Context, id, name, mbid string) (string, error) {
	if a.impl == nil {
		return "", errors.New("MCP agent implementation is nil")
	}
	// Construct args and call the implementation's specific tool caller
	args := ArtistArgs{ID: id, Name: name, Mbid: mbid}
	return a.impl.callMCPTool(ctx, McpToolNameGetURL, args)
}

// Note: A Close method on MCPAgent itself isn't part of agents.Interface.
// Cleanup of the specific implementation happens via impl.Close().
// Cleanup of shared Wazero resources needs separate handling (e.g., on app shutdown).

// ArtistArgs defines the structure for MCP tool arguments requiring artist info.
type ArtistArgs struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mbid string `json:"mbid,omitempty"`
}

var _ agents.ArtistBiographyRetriever = (*MCPAgent)(nil)
var _ agents.ArtistURLRetriever = (*MCPAgent)(nil)

func init() {
	agents.Register(McpAgentName, mcpConstructor)
}
