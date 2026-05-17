package tools

import (
	"context"
	_ "embed"
	"fmt"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/memory/engine"
)

//go:embed reflect.md
var reflectDescription []byte

const ReflectToolName = "reflect"

type ReflectParams struct {
	Query     string `json:"query" description:"What to reflect on — a question about past sessions, decisions, or project history"`
	Scope     string `json:"scope,omitempty" description:"Optional scope filter: session, project, user, or global"`
	Kind      string `json:"kind,omitempty" description:"Optional kind filter: preference, decision, procedure, pitfall, reference, or task_state"`
	SessionID string `json:"session_id,omitempty" description:"Optional session ID to scope the reflection to a specific session"`
}

func NewReflectTool(retriever engine.Retriever) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ReflectToolName,
		string(reflectDescription),
		func(ctx context.Context, params ReflectParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if retriever == nil {
				return fantasy.NewTextErrorResponse("Memory engine retriever is not available. Enable the memory engine to use reflect."), nil
			}

			opts := map[string]any{}
			if params.Scope != "" {
				opts["scope"] = params.Scope
			}
			if params.Kind != "" {
				opts["kind"] = params.Kind
			}
			if params.SessionID != "" {
				opts["session_id"] = params.SessionID
			}

			result, err := retriever.Reflect(ctx, params.Query, opts)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Reflect failed: %s", err.Error())), nil
			}
			if result == "" {
				return fantasy.NewTextResponse("No relevant memories found to reflect on."), nil
			}

			return fantasy.NewTextResponse(result), nil
		},
	)
}
