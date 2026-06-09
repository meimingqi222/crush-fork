package tools

import (
	"context"
	_ "embed"
	"fmt"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/memory/engine"
)

const CrushToolName = "crush"

//go:embed crush.md
var crushDescription []byte

// CrushParams is the unified parameter struct for the consolidated crush tool.
type CrushParams struct {
	Action string `json:"action" description:"The action to perform. One of: info (show current Crush instance status), logs (show recent log entries)."`
	Lines  int    `json:"lines,omitempty" description:"Number of recent log entries to return, default 50, max 100 (logs action only)."`
}

// NewCrushTool creates the consolidated crush tool that replaces crush_info and crush_logs.
func NewCrushTool(cfg *config.ConfigStore, lspManager *lsp.Manager, memEng *engine.Engine, logFile string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		CrushToolName,
		string(crushDescription),
		func(ctx context.Context, params CrushParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			switch params.Action {
			case "info":
				return fantasy.NewTextResponse(buildCrushInfo(ctx, cfg, lspManager, memEng)), nil
			case "logs":
				result := runCrushLogs(logFile, CrushLogsParams{Lines: params.Lines})
				return fantasy.NewTextResponse(result), nil
			default:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown crush action: %q. Valid actions: info, logs", params.Action)), nil
			}
		},
	)
}
