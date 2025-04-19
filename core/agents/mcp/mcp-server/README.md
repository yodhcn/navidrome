# MCP Server (Proof of Concept)

This directory contains the source code for the `mcp-server`, a simple server implementation used as a proof-of-concept (PoC) for the Navidrome Plugin/MCP agent system.

This server is designed to be compiled into a WebAssembly (WASM) module (`.wasm`) using the `wasip1` target.

## Compilation

To compile the server into a WASM module (`mcp-server.wasm`), navigate to this directory in your terminal and run the following command:

```bash
CGO_ENABLED=0 GOOS=wasip1 GOARCH=wasm go build -o mcp-server.wasm .
```

**Note:** This command compiles the WASM module _without_ the `netgo` tag. Networking operations (like HTTP requests) are expected to be handled by host functions provided by the embedding application (Navidrome's `MCPAgent`) rather than directly within the WASM module itself.

Place the resulting `mcp-server.wasm` file where the Navidrome `MCPAgent` expects it (currently configured via the `McpServerPath` constant in `core/agents/mcp/mcp_agent.go`).
