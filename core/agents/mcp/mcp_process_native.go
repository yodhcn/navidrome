package mcp

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/navidrome/navidrome/log"
)

// startNativeProcess starts the MCP server as a native executable.
func (a *MCPAgent) startNativeProcess(ctx context.Context) (stdin io.WriteCloser, stdout io.ReadCloser, cmd *exec.Cmd, err error) {
	log.Debug(ctx, "Starting native MCP server process", "path", McpServerPath)
	cmd = exec.CommandContext(context.Background(), McpServerPath) // Use Background context for long-running process

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
		// Use the central cleanup function which handles nil checks.
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
