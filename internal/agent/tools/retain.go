package tools

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/google/uuid"
)

//go:embed retain.md
var retainDescription []byte

const RetainToolName = "retain"

type RetainParams struct {
	Scope      string   `json:"scope" description:"Memory scope: session, project, user, or global"`
	Kind       string   `json:"kind" description:"Memory kind: preference, decision, procedure, pitfall, reference, or task_state"`
	Content    string   `json:"content" description:"Full memory content"`
	Summary    string   `json:"summary,omitempty" description:"Optional short summary of the memory"`
	Tags       []string `json:"tags,omitempty" description:"Optional tags for categorization"`
	Importance float64  `json:"importance,omitempty" description:"Importance from 0.0 to 1.0 (default 0.5)"`
}

type memoryMaterializer interface {
	TriggerMaterialization(ctx context.Context) error
}

// NewRetainTool creates the retain tool. Unlike file-editing tools, retain
// writes to crush's own data directory (the memory event log), not the
// user's workspace, so it does not go through a permission prompt -- mirrors
// oh-my-pi's retain tool, which is registered with approval: "read". The
// tool description carries the compensating safeguard instead (do not
// retain secrets/credentials).
func NewRetainTool(eventStore engine.EventStore, workingDir string, materializers ...memoryMaterializer) fantasy.AgentTool {
	var materializer memoryMaterializer
	if len(materializers) > 0 {
		materializer = materializers[0]
	}
	return fantasy.NewAgentTool(
		RetainToolName,
		string(retainDescription),
		func(ctx context.Context, params RetainParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if eventStore == nil {
				return fantasy.NewTextErrorResponse("Memory engine is not available. Enable the memory engine to use retain."), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for retain")
			}

			importance := params.Importance
			if importance <= 0 {
				importance = 0.5
			}

			event := engine.MemoryEvent{
				ID:      uuid.New().String(),
				Scope:   engine.MemoryScope(params.Scope),
				Kind:    engine.MemoryKind(params.Kind),
				Content: params.Content,
				Summary: params.Summary,
				Source: engine.MemorySourceRef{
					SessionID: sessionID,
					CWD:       workingDir,
				},
				Confidence: 0.8,
				Importance: importance,
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
				Tags:       params.Tags,
			}

			if err := eventStore.Append(ctx, event); err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to retain memory: %s", err.Error())), nil
			}
			if materializer != nil && engine.IsMaterializableEvent(event) {
				if err := materializer.TriggerMaterialization(ctx); err != nil {
					slog.Warn("Retain memory materialization failed", "error", err, "session_id", sessionID)
				}
			}

			return fantasy.NewTextResponse(
				fmt.Sprintf("Retained memory event [%s/%s] (ID: %s)", params.Scope, params.Kind, event.ID),
			), nil
		},
	)
}
