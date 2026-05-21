package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/usage/selfheal"
)

// selfhealMCPCmd is the bridge between claude's MCP client and the
// daemon-side BridgeServer. Spawned by claude -p (configured via
// --mcp-config) during a self-heal session. Reads MCP JSON-RPC on
// stdin, writes responses on stdout; each tool handler relays the
// call over a unix-socket to the daemon's running Session.
//
// Hidden / underscored on purpose: this isn't a user-facing command.
var selfhealMCPCmd = &cobra.Command{
	Use:    "_selfheal_mcp",
	Short:  "Internal: MCP bridge launched by claude -p during self-heal",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		sockPath := os.Getenv("BLOODHOUND_SELFHEAL_SOCK")
		if sockPath == "" {
			return fmt.Errorf("BLOODHOUND_SELFHEAL_SOCK is not set; this subcommand is only meant to be launched by the daemon's self-heal path")
		}
		bridge, err := selfheal.DialBridge(sockPath)
		if err != nil {
			return fmt.Errorf("dial bridge: %w", err)
		}
		defer bridge.Close()

		mcpServer := server.NewMCPServer("bloodhound-selfheal", "0.1.0")

		mcpServer.AddTool(
			mcp.NewTool(selfheal.ToolReadPTY,
				mcp.WithDescription("Wait for the pty to be quiet for settle_ms milliseconds, then return the current VT-rendered grid. Use a longer settle_ms (e.g. 800) when you expect a panel to be rendering, shorter (e.g. 200) when you just want a quick peek."),
				mcp.WithInteger("settle_ms", mcp.Description("milliseconds to wait for the pty to be idle before snapshotting"), mcp.Required()),
			),
			relayHandler(bridge, selfheal.ToolReadPTY, func(r mcp.CallToolRequest) (any, error) {
				return selfheal.ReadPTYRequest{SettleMs: r.GetInt("settle_ms", 400)}, nil
			}),
		)

		mcpServer.AddTool(
			mcp.NewTool(selfheal.ToolSendKeys,
				mcp.WithDescription(`Write text into the pty. Use "\r" for Enter, "\x03" for Ctrl-C. Typing "/usage\r" submits the /usage command from claude's main input prompt.`),
				mcp.WithString("text", mcp.Description("text to type, with escape sequences honoured"), mcp.Required()),
			),
			relayHandler(bridge, selfheal.ToolSendKeys, func(r mcp.CallToolRequest) (any, error) {
				text, err := r.RequireString("text")
				if err != nil {
					return nil, err
				}
				return selfheal.SendKeysRequest{Text: text}, nil
			}),
		)

		mcpServer.AddTool(
			mcp.NewTool(selfheal.ToolTestRegex,
				mcp.WithDescription("Compile pattern as a Go RE2 regex and search the current rendered grid. Returns the capture group's value plus ±60 chars of context. RE2 only — no backreferences, no lookarounds. Use (?is) for case-insensitive + DOTALL."),
				mcp.WithString("pattern", mcp.Description("RE2 regex"), mcp.Required()),
				mcp.WithInteger("group", mcp.Description("capture group to return (0 = whole match)"), mcp.Required()),
				mcp.WithString("field", mcp.Description("label echoed back; helps you keep track of which field this test was for")),
			),
			relayHandler(bridge, selfheal.ToolTestRegex, func(r mcp.CallToolRequest) (any, error) {
				pat, err := r.RequireString("pattern")
				if err != nil {
					return nil, err
				}
				return selfheal.TestRegexRequest{
					Pattern: pat,
					Group:   r.GetInt("group", 0),
					Field:   r.GetString("field", ""),
				}, nil
			}),
		)

		mcpServer.AddTool(
			mcp.NewTool(selfheal.ToolSaveExtractor,
				mcp.WithDescription(`Persist a set of FieldRules as the active extractor. Each rule is {name, type ("int"|"string"), regex, group, required}. The server validates the schema, dry-runs against the current grid, and only saves if every required field extracts. Returns ok=true on success; on failure returns missing[] so you can refine and retry.`),
				mcp.WithArray("fields", mcp.Description("list of FieldRule objects"), mcp.Required()),
			),
			relayHandler(bridge, selfheal.ToolSaveExtractor, func(r mcp.CallToolRequest) (any, error) {
				var req selfheal.SaveExtractorRequest
				if err := r.BindArguments(&req); err != nil {
					return nil, err
				}
				return req, nil
			}),
		)

		// Run until stdin EOF (claude -p closed its end) or signal.
		return server.ServeStdio(mcpServer)
	},
}

// relayHandler builds an MCP tool handler that decodes the request,
// hands it to the bridge as a daemon-side tool call, and returns the
// bridge's response as a structured MCP result. Errors are returned as
// MCP tool errors so claude sees them in-band.
func relayHandler(bridge *selfheal.BridgeClient, tool string, decode func(mcp.CallToolRequest) (any, error)) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decode(request)
		if err != nil {
			return mcp.NewToolResultErrorf("bad arguments: %v", err), nil
		}
		raw, err := bridge.Call(tool, args)
		if err != nil {
			return mcp.NewToolResultErrorf("daemon: %v", err), nil
		}
		// Return the structured result, plus a text fallback so claude
		// can read the JSON literally if needed.
		var anyResult map[string]any
		if uerr := json.Unmarshal(raw, &anyResult); uerr != nil {
			// Result wasn't a JSON object — return it as text.
			return mcp.NewToolResultText(string(raw)), nil
		}
		return mcp.NewToolResultStructured(anyResult, string(raw)), nil
	}
}

func init() {
	rootCmd.AddCommand(selfhealMCPCmd)
}
