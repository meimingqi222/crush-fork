package tools

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"
)

//go:embed web_fetch.md
var webFetchToolDescription []byte

// NewWebFetchTool creates a simple web fetch tool for sub-agents (no permissions needed).
func NewWebFetchTool(workingDir string, client *http.Client) fantasy.AgentTool {
	if client == nil {
		client = NewSafeHTTPClient(30 * time.Second)
	}

	return fantasy.NewParallelAgentTool(
		WebFetchToolName,
		string(webFetchToolDescription),
		func(ctx context.Context, params WebFetchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.URL == "" {
				return fantasy.NewTextErrorResponse("url is required"), nil
			}

			content, err := ReadURLAndConvert(ctx, client, params.URL)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to fetch URL: %s", err)), nil
			}

			hasLargeContent := len(content) > LargeContentThreshold
			var result strings.Builder

			if hasLargeContent {
				// Use URL-based filename for better predictability
				tempFilePath := filepath.Join(workingDir, fmt.Sprintf("fetch-%x.md", time.Now().UnixNano()))
				tempFile, err := os.Create(tempFilePath)
				if err != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to create temporary file: %s", err)), nil
				}

				if _, err := tempFile.WriteString(content); err != nil {
					_ = tempFile.Close()
					_ = os.Remove(tempFilePath) // Clean up on write failure
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to write content to file: %s", err)), nil
				}
				if err := tempFile.Close(); err != nil {
					_ = os.Remove(tempFilePath) // Clean up on close failure
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to close temporary file: %s", err)), nil
				}

				fmt.Fprintf(&result, "Fetched content from %s (large page)\n\n", params.URL)
				fmt.Fprintf(&result, "Content saved to: %s\n\n", tempFilePath)
				result.WriteString("Use the read and grep tools to analyze this file.")
			} else {
				fmt.Fprintf(&result, "Fetched content from %s:\n\n", params.URL)
				result.WriteString(content)
			}

			return fantasy.NewTextResponse(result.String()), nil
		})
}
