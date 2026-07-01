package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/trail"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage/selfheal"
)

// trailMCPCmd is the MCP bridge claude spawns during a Trail analysis
// (configured via --mcp-config). It exposes the single save_trail_brief
// tool and relays the call to the daemon's running trail bridge over the
// unix socket named in BLOODHOUND_TRAIL_SOCK. Hidden; not user-facing.
var trailMCPCmd = &cobra.Command{
	Use:    "_trail_mcp",
	Short:  "Internal: MCP bridge launched by claude during Trail analysis",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		sockPath := os.Getenv("BLOODHOUND_TRAIL_SOCK")
		if sockPath == "" {
			return fmt.Errorf("BLOODHOUND_TRAIL_SOCK is not set; this subcommand is only meant to be launched by the daemon's trail path")
		}
		bridge, err := selfheal.DialBridge(sockPath)
		if err != nil {
			return fmt.Errorf("dial bridge: %w", err)
		}
		defer bridge.Close()

		mcpServer := server.NewMCPServer(trail.MCPServerName, "0.1.0")
		mcpServer.AddTool(
			mcp.NewTool(trail.ToolSaveBrief,
				mcp.WithDescription("Persist your summary of one Claude Code session: a short headline, a 1-3 sentence summary, the worktrees it touched (path + role primary|incidental), and its open loops (key, text, status active|blocked|waiting|done, optional related_repo_path). Call exactly once, then stop."),
				mcp.WithString("headline", mcp.Description("<=60 char scannable title"), mcp.Required()),
				mcp.WithString("summary", mcp.Description("1-3 sentence summary of what the session is doing"), mcp.Required()),
				mcp.WithArray("repos", mcp.Description("worktrees touched: objects {path, role}")),
				mcp.WithArray("open_loops", mcp.Description("outstanding items: objects {key, text, status, related_repo_path}")),
			),
			func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args trail.SaveBriefArgs
				if err := request.BindArguments(&args); err != nil {
					return mcp.NewToolResultErrorf("bad arguments: %v", err), nil
				}
				raw, err := bridge.Call(trail.ToolSaveBrief, args)
				if err != nil {
					return mcp.NewToolResultErrorf("daemon: %v", err), nil
				}
				var res map[string]any
				if uerr := json.Unmarshal(raw, &res); uerr != nil {
					return mcp.NewToolResultText(string(raw)), nil
				}
				return mcp.NewToolResultStructured(res, string(raw)), nil
			},
		)
		return server.ServeStdio(mcpServer)
	},
}

func init() {
	rootCmd.AddCommand(trailMCPCmd)
}
