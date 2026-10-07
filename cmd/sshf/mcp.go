package main

import "github.com/spf13/cobra"

func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "以 stdio 模式启动 MCP server(供 AI Agent 使用)",
		RunE:  notImplemented("mcp"),
	}
}
