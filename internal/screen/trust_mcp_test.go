package screen

import "testing"

// Claude Code asks before it runs a server a project's .mcp.json names. That
// dialog is a trust screen too: Enter there would approve the command.
func TestClaudeProjectMcpApprovalIsATrustScreen(t *testing.T) {
	for name, s := range map[string]string{
		"plain":   "╭────╮\n│ New MCP server found in this project: ctx7\n│ \x1b[1m❯\x1b[0m 1. Use this and all future MCP servers in this project\n│   2. Use this MCP server\n│   3. Continue without using this MCP server\n╰────╯\n",
		"capture": "\x1b[1CNew\x1b[1CMCP\x1b[1Cserver\x1b[1Cfound\x1b[1Cin\x1b[1C.mcp.json:\x1b[1Cctx7\r\n\x1b[1C❯\x1b[1C1.\x1b[1CUse\x1b[1Cthis\x1b[1CMCP\x1b[1Cserver\r\n",
	} {
		if got, ok := TrustScreen("claude", s); !ok || got != "New MCP server found" {
			t.Errorf("%s: got %q %v", name, got, ok)
		}
	}
}
