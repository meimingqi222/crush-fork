package tools

import (
	"context"
	_ "embed"
	"fmt"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/memory/engine"
)

//go:embed recall.md
var recallDescription []byte

const RecallToolName = "recall"

type RecallParams struct {
	Query string `json:"query,omitempty" description:"Optional search query for targeted recall; omit to get the full materialized summary"`
	Scope string `json:"scope,omitempty" description:"Optional scope filter: session, project, user, or global"`
	Kind  string `json:"kind,omitempty" description:"Optional kind filter: preference, decision, procedure, pitfall, reference, or task_state"`
	Limit int    `json:"limit,omitempty" description:"Maximum number of events to return (default 20)"`
}

func NewRecallTool(retriever engine.Retriever, eventStore engine.EventStore) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		RecallToolName,
		string(recallDescription),
		func(ctx context.Context, params RecallParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if retriever == nil {
				return fantasy.NewTextErrorResponse("Memory engine retriever is not available. Enable the memory engine to use recall."), nil
			}

			sessionID := GetSessionFromContext(ctx)

			if params.Query != "" || params.Scope != "" || params.Kind != "" {
				opts := map[string]any{}
				if params.Scope != "" {
					opts["scope"] = params.Scope
				}
				if params.Kind != "" {
					opts["kind"] = params.Kind
				}
				if sessionID != "" {
					opts["session_id"] = sessionID
				}
				if params.Limit > 0 {
					opts["limit"] = params.Limit
				}

				events, err := retriever.Retrieve(ctx, params.Query, opts)
				if err != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Recall failed: %s", err.Error())), nil
				}
				if len(events) == 0 {
					return fantasy.NewTextResponse("No matching memory events found."), nil
				}

				formatted := formatMemoryEvents(events)
				return fantasy.NewTextResponse(formatted), nil
			}

			opts := map[string]any{}
			if sessionID != "" {
				opts["session_id"] = sessionID
			}

			summary, err := retriever.Recall(ctx, opts)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Recall failed: %s", err.Error())), nil
			}
			if summary == "" {
				return fantasy.NewTextResponse("No materialized memory available."), nil
			}
			return fantasy.NewTextResponse(summary), nil
		},
	)
}

func formatMemoryEvents(events []engine.MemoryEvent) string {
	var result string
	for _, evt := range events {
		summary := evt.Summary
		if summary == "" {
			summary = truncateString(evt.Content, 200)
		}
		result += fmt.Sprintf("[%s/%s] %s\n", evt.Scope, evt.Kind, summary)
		if evt.Content != "" && evt.Content != summary {
			result += fmt.Sprintf("  Content: %s\n", truncateString(evt.Content, 200))
		}
		if evt.Source.SessionID != "" {
			result += fmt.Sprintf("  Session: %s\n", evt.Source.SessionID)
		}
		result += fmt.Sprintf("  Confidence: %.0f%% | Importance: %.1f\n",
			evt.Confidence*100, evt.Importance)
		if len(evt.Tags) > 0 {
			tags := ""
			for i, tag := range evt.Tags {
				if i > 0 {
					tags += ", "
				}
				tags += tag
			}
			result += fmt.Sprintf("  Tags: %s\n", tags)
		}
		result += "\n"
	}
	return result
}

func truncateString(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
