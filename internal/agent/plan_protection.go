package agent

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plan"
)

type readToolMetadata struct {
	Path string `json:"path"`
}

// planCompactionProtector returns a predicate that keeps plan file read results
// intact during context pruning and compaction.
func planCompactionProtector(workspaceRoot, sessionID, planFilePath string) func(message.ToolResult) bool {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	sessionID = strings.TrimSpace(sessionID)
	planFilePath = strings.TrimSpace(planFilePath)

	var protectedPaths []string
	if planFilePath != "" {
		protectedPaths = append(protectedPaths, planFilePath)
		if clean, err := filepath.Abs(planFilePath); err == nil {
			protectedPaths = append(protectedPaths, clean)
		}
	}
	if workspaceRoot != "" && sessionID != "" {
		if canonical := plan.PlanFilePath(workspaceRoot, sessionID, "plan"); canonical != "" {
			protectedPaths = append(protectedPaths, canonical)
			if clean, err := filepath.Abs(canonical); err == nil {
				protectedPaths = append(protectedPaths, clean)
			}
		}
	}

	return func(tr message.ToolResult) bool {
		if tr.Name != tools.ReadToolName {
			return false
		}
		path := readToolResultPath(tr)
		if path == "" {
			return false
		}
		if plan.IsLocalPlanURI(path) {
			return true
		}
		cleanPath, err := filepath.Abs(path)
		if err != nil {
			cleanPath = filepath.Clean(path)
		}
		for _, protected := range protectedPaths {
			if protected == "" {
				continue
			}
			protectedClean, err := filepath.Abs(protected)
			if err != nil {
				protectedClean = filepath.Clean(protected)
			}
			if cleanPath == protectedClean {
				return true
			}
			if workspaceRoot != "" && sessionID != "" {
				if slug, ok := plan.SlugFromPlanPath(workspaceRoot, sessionID, cleanPath); ok && slug != "" {
					return true
				}
			}
		}
		return false
	}
}

func readToolResultPath(tr message.ToolResult) string {
	if tr.Metadata == "" {
		return ""
	}
	var meta readToolMetadata
	if err := json.Unmarshal([]byte(tr.Metadata), &meta); err != nil {
		return ""
	}
	return strings.TrimSpace(meta.Path)
}
