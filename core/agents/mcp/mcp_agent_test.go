package mcp_test

import (
	"context"
	"errors"
	"fmt"
	"io"

	mcp_client "github.com/metoro-io/mcp-golang" // Renamed alias for clarity
	"github.com/navidrome/navidrome/core/agents"
	"github.com/navidrome/navidrome/core/agents/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Define the mcpClient interface locally for mocking, matching the one
// used internally by MCPNative/MCPWasm.
type mcpClient interface {
	Initialize(ctx context.Context) (*mcp_client.InitializeResponse, error)
	CallTool(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error)
}

// mockMCPClient is a mock implementation of mcpClient for testing.
type mockMCPClient struct {
	InitializeFunc func(ctx context.Context) (*mcp_client.InitializeResponse, error)
	CallToolFunc   func(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error)
	callToolArgs   []any  // Store args for verification
	callToolName   string // Store tool name for verification
}

func (m *mockMCPClient) Initialize(ctx context.Context) (*mcp_client.InitializeResponse, error) {
	if m.InitializeFunc != nil {
		return m.InitializeFunc(ctx)
	}
	return &mcp_client.InitializeResponse{}, nil // Default success
}

func (m *mockMCPClient) CallTool(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error) {
	m.callToolName = toolName
	m.callToolArgs = append(m.callToolArgs, args)
	if m.CallToolFunc != nil {
		return m.CallToolFunc(ctx, toolName, args)
	}
	return &mcp_client.ToolResponse{}, nil
}

// Ensure mock implements the local interface (compile-time check)
var _ mcpClient = (*mockMCPClient)(nil)

var _ = Describe("MCPAgent", func() {
	var (
		ctx context.Context
		// We test the public MCPAgent wrapper, which uses the implementations internally.
		// The actual agent instance might be native or wasm depending on McpServerPath
		agent      agents.Interface // Use the public agents.Interface
		mockClient *mockMCPClient
	)

	BeforeEach(func() {
		ctx = context.Background()
		mockClient = &mockMCPClient{
			callToolArgs: make([]any, 0), // Reset args on each test
		}

		// Instantiate the real agent using a testing constructor
		// This constructor needs to be added to the mcp package.
		agent = mcp.NewAgentForTesting(mockClient)
		Expect(agent).NotTo(BeNil(), "Agent should be created")
	})

	// Helper to get the concrete agent type for calling specific methods
	getConcreteAgent := func() *mcp.MCPAgent {
		concreteAgent, ok := agent.(*mcp.MCPAgent)
		Expect(ok).To(BeTrue(), "Agent should be of type *mcp.MCPAgent")
		return concreteAgent
	}

	Describe("GetArtistBiography", func() {
		It("should call the correct tool and return the biography", func() {
			expectedBio := "This is the artist bio."
			mockClient.CallToolFunc = func(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error) {
				Expect(toolName).To(Equal(mcp.McpToolNameGetBio))
				Expect(args).To(BeAssignableToTypeOf(mcp.ArtistArgs{}))
				typedArgs := args.(mcp.ArtistArgs)
				Expect(typedArgs.ID).To(Equal("id1"))
				Expect(typedArgs.Name).To(Equal("Artist Name"))
				Expect(typedArgs.Mbid).To(Equal("mbid1"))
				return mcp_client.NewToolResponse(mcp_client.NewTextContent(expectedBio)), nil
			}

			bio, err := getConcreteAgent().GetArtistBiography(ctx, "id1", "Artist Name", "mbid1")
			Expect(err).NotTo(HaveOccurred())
			Expect(bio).To(Equal(expectedBio))
		})

		It("should return error if CallTool fails", func() {
			expectedErr := errors.New("mcp tool error")
			mockClient.CallToolFunc = func(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error) {
				return nil, expectedErr
			}

			bio, err := getConcreteAgent().GetArtistBiography(ctx, "id1", "Artist Name", "mbid1")
			Expect(err).To(HaveOccurred())
			// The error originates from the implementation now, check for specific part
			Expect(err.Error()).To(SatisfyAny(
				ContainSubstring("native MCP agent not ready"), // Error from native
				ContainSubstring("WASM MCP agent not ready"),   // Error from WASM
				ContainSubstring("failed to call native MCP tool"),
				ContainSubstring("failed to call WASM MCP tool"),
			))
			Expect(errors.Is(err, expectedErr)).To(BeTrue())
			Expect(bio).To(BeEmpty())
		})

		It("should return ErrNotFound if CallTool response is empty", func() {
			mockClient.CallToolFunc = func(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error) {
				// Return a response created with no content parts
				return mcp_client.NewToolResponse(), nil
			}

			bio, err := getConcreteAgent().GetArtistBiography(ctx, "id1", "Artist Name", "mbid1")
			Expect(err).To(MatchError(agents.ErrNotFound))
			Expect(bio).To(BeEmpty())
		})

		It("should return ErrNotFound if CallTool response has nil TextContent (simulated by empty string)", func() {
			mockClient.CallToolFunc = func(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error) {
				// Simulate nil/empty text content by creating response with empty string text
				return mcp_client.NewToolResponse(mcp_client.NewTextContent("")), nil
			}

			bio, err := getConcreteAgent().GetArtistBiography(ctx, "id1", "Artist Name", "mbid1")
			Expect(err).To(MatchError(agents.ErrNotFound))
			Expect(bio).To(BeEmpty())
		})

		It("should return comm error if CallTool returns pipe error", func() {
			mockClient.CallToolFunc = func(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error) {
				return nil, io.ErrClosedPipe
			}

			bio, err := getConcreteAgent().GetArtistBiography(ctx, "id1", "Artist Name", "mbid1")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(SatisfyAny(
				ContainSubstring("native MCP agent process communication error"),
				ContainSubstring("WASM MCP agent module communication error"),
			))
			Expect(errors.Is(err, io.ErrClosedPipe)).To(BeTrue())
			Expect(bio).To(BeEmpty())
		})

		It("should return ErrNotFound if MCP tool returns an error string", func() {
			mcpErrorString := "handler returned an error: something went wrong on the server"
			mockClient.CallToolFunc = func(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error) {
				return mcp_client.NewToolResponse(mcp_client.NewTextContent(mcpErrorString)), nil
			}

			bio, err := getConcreteAgent().GetArtistBiography(ctx, "id1", "Artist Name", "mbid1")
			Expect(err).To(MatchError(agents.ErrNotFound))
			Expect(bio).To(BeEmpty())
		})
	})

	Describe("GetArtistURL", func() {
		It("should call the correct tool and return the URL", func() {
			expectedURL := "http://example.com/artist"
			mockClient.CallToolFunc = func(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error) {
				Expect(toolName).To(Equal(mcp.McpToolNameGetURL))
				Expect(args).To(BeAssignableToTypeOf(mcp.ArtistArgs{}))
				typedArgs := args.(mcp.ArtistArgs)
				Expect(typedArgs.ID).To(Equal("id2"))
				Expect(typedArgs.Name).To(Equal("Another Artist"))
				Expect(typedArgs.Mbid).To(Equal("mbid2"))
				return mcp_client.NewToolResponse(mcp_client.NewTextContent(expectedURL)), nil
			}

			url, err := getConcreteAgent().GetArtistURL(ctx, "id2", "Another Artist", "mbid2")
			Expect(err).NotTo(HaveOccurred())
			Expect(url).To(Equal(expectedURL))
		})

		It("should return error if CallTool fails", func() {
			expectedErr := errors.New("mcp tool error url")
			mockClient.CallToolFunc = func(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error) {
				return nil, expectedErr
			}

			url, err := getConcreteAgent().GetArtistURL(ctx, "id2", "Another Artist", "mbid2")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(SatisfyAny(
				ContainSubstring("native MCP agent not ready"), // Error from native
				ContainSubstring("WASM MCP agent not ready"),   // Error from WASM
				ContainSubstring("failed to call native MCP tool"),
				ContainSubstring("failed to call WASM MCP tool"),
			))
			Expect(errors.Is(err, expectedErr)).To(BeTrue())
			Expect(url).To(BeEmpty())
		})

		It("should return ErrNotFound if CallTool response is empty", func() {
			mockClient.CallToolFunc = func(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error) {
				// Return a response created with no content parts
				return mcp_client.NewToolResponse(), nil
			}

			url, err := getConcreteAgent().GetArtistURL(ctx, "id2", "Another Artist", "mbid2")
			Expect(err).To(MatchError(agents.ErrNotFound))
			Expect(url).To(BeEmpty())
		})

		It("should return comm error if CallTool returns pipe error", func() {
			mockClient.CallToolFunc = func(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error) {
				return nil, fmt.Errorf("write: %w", io.ErrClosedPipe)
			}

			url, err := getConcreteAgent().GetArtistURL(ctx, "id2", "Another Artist", "mbid2")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(SatisfyAny(
				ContainSubstring("native MCP agent process communication error"),
				ContainSubstring("WASM MCP agent module communication error"),
			))
			Expect(errors.Is(err, io.ErrClosedPipe)).To(BeTrue())
			Expect(url).To(BeEmpty())
		})

		It("should return ErrNotFound if MCP tool returns an error string", func() {
			mcpErrorString := "handler returned an error: could not find url"
			mockClient.CallToolFunc = func(ctx context.Context, toolName string, args any) (*mcp_client.ToolResponse, error) {
				return mcp_client.NewToolResponse(mcp_client.NewTextContent(mcpErrorString)), nil
			}

			url, err := getConcreteAgent().GetArtistURL(ctx, "id2", "Another Artist", "mbid2")
			Expect(err).To(MatchError(agents.ErrNotFound))
			Expect(url).To(BeEmpty())
		})
	})
})
