//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	fantasy "charm.land/fantasy"
)

func main() {
	workDir := "/tmp"
	allTools := []fantasy.AgentTool{
		tools.NewBashToolWithSessions(nil, nil, workDir, nil, "claude", nil),
		tools.NewDownloadTool(nil, workDir, nil),
		tools.NewEditTool(nil, nil, nil, tools.FileTracker{}, workDir),
		tools.NewFetchTool(nil, workDir, nil),
		tools.NewGlobTool(workDir),
		tools.NewGrepTool(workDir, config.ToolGrep{}),
		tools.NewSourcegraphTool(nil),
		tools.NewViewTool(nil, nil, tools.FileTracker{}, workDir, config.ToolLs{}),
		tools.NewWriteTool(nil, nil, nil, tools.FileTracker{}, workDir),
	}

	type toolJSON struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"input_schema"`
	}

	var result []toolJSON
	for _, t := range allTools {
		info := t.Info()
		inputSchema := map[string]any{
			"type":       "object",
			"properties": info.Parameters,
			"required":   info.Required,
		}
		result = append(result, toolJSON{
			Name:        info.Name,
			Description: info.Description,
			InputSchema: inputSchema,
		})
	}

	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(b))
}
