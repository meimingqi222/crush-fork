package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/toolruntime"
)

//go:embed subtask_result.md
var subtaskResultDescription []byte

const SubtaskResultToolName = "subtask_result"

type SubtaskResultParams struct {
	TaskRef   string `json:"task_ref,omitempty" description:"Optional short task reference from an Agent tool result, such as 0-review or subtask://0-review. Prefer this over long child session IDs when available."`
	SessionID string `json:"session_id,omitempty" description:"Optional child session ID from a previous Agent tool call. Omit to use the most recent child session in the current conversation."`
	AgentID   string `json:"agent_id,omitempty" description:"The agent ID from a background agent (alternative to session_id)"`
	Offset    int    `json:"offset,omitempty" description:"Line offset to start from (0-based, for paginating long outputs)"`
	Limit     int    `json:"limit,omitempty" description:"Maximum number of characters to return (default 40000, max 80000)"`
}

func NewSubtaskResultTool(messages message.Service) fantasy.AgentTool {
	const defaultLimit = 40_000
	const maxLimit = 80_000

	return fantasy.NewAgentTool(
		SubtaskResultToolName,
		string(subtaskResultDescription),
		func(ctx context.Context, params SubtaskResultParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			limit := params.Limit
			if limit <= 0 {
				limit = defaultLimit
			}
			if limit > maxLimit {
				limit = maxLimit
			}

			// Check for background agent lookup first.
			agentID := strings.TrimSpace(params.AgentID)
			if agentID != "" {
				return subtaskResultFromBackgroundAgent(ctx, messages, agentID, params.Offset, limit)
			}

			// Fall back to session-based lookup.
			sessionID := strings.TrimSpace(params.SessionID)
			if messages == nil {
				return fantasy.ToolResponse{}, fmt.Errorf("message service is not configured")
			}
			if taskRef := normalizeSubtaskTaskRef(params.TaskRef); taskRef != "" {
				target, ok := resolveSubtaskTargetByTaskRef(ctx, messages, taskRef)
				if !ok {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("task_ref %q was not found in the current conversation", taskRef)), nil
				}
				return subtaskResultFromTarget(ctx, messages, target, params.Offset, limit), nil
			}
			if sessionID == "" || isUnresolvedSubtaskSessionPlaceholder(sessionID) {
				if target, ok := inferLatestSubtaskTarget(ctx, messages); ok {
					return subtaskResultFromTarget(ctx, messages, target, params.Offset, limit), nil
				}
			}
			if sessionID == "" {
				return fantasy.NewTextErrorResponse("session_id or agent_id is required, and no child session could be inferred from the current conversation"), nil
			}

			return subtaskResultFromTarget(ctx, messages, subtaskResultTarget{SessionID: sessionID}, params.Offset, limit), nil
		},
	)
}

type subtaskResultTarget struct {
	SessionID     string
	TaskRef       string
	Preview       string
	Status        message.ToolResultSubtaskStatus
	HasFullOutput bool
}

func subtaskResultFromTarget(ctx context.Context, messages message.Service, target subtaskResultTarget, offset, limit int) fantasy.ToolResponse {
	sessionID := strings.TrimSpace(target.SessionID)
	if sessionID == "" {
		if strings.TrimSpace(target.Preview) == "" {
			return fantasy.NewTextResponse("No child session is available for this subtask.")
		}
		return fantasy.NewTextResponse(paginateSubtaskResult(formatSubtaskPreview(target), offset, limit))
	}

	msgs, err := messages.List(ctx, sessionID)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to load session %s: %s", sessionID, err))
	}

	if text, ok := latestAssistantText(msgs); ok {
		return fantasy.NewTextResponse(paginateSubtaskResult(formatSubtaskSessionText(sessionID, target.TaskRef, text), offset, limit))
	}
	if text, ok := latestSessionFallbackText(msgs); ok {
		return fantasy.NewTextResponse(paginateSubtaskResult(formatSubtaskSessionText(sessionID, target.TaskRef, text), offset, limit))
	}
	if resp, fallbackOK := subtaskResultFromBackgroundAgentIfFound(ctx, messages, sessionID, offset, limit); fallbackOK {
		return resp
	}
	if strings.TrimSpace(target.Preview) != "" {
		return fantasy.NewTextResponse(paginateSubtaskResult(formatSubtaskPreview(target), offset, limit))
	}
	return fantasy.NewTextResponse(fmt.Sprintf("No assistant response found in session %s", sessionID))
}

func subtaskResultFromBackgroundAgent(ctx context.Context, messages message.Service, agentID string, offset, limit int) (fantasy.ToolResponse, error) {
	resp, ok := subtaskResultFromBackgroundAgentIfFound(ctx, messages, agentID, offset, limit)
	if !ok {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Background agent %q not found", agentID)), nil
	}
	return resp, nil
}

func subtaskResultFromBackgroundAgentIfFound(ctx context.Context, messages message.Service, agentID string, offset, limit int) (fantasy.ToolResponse, bool) {
	lookup := toolruntime.BackgroundAgentLookupFromContext(ctx)
	if lookup == nil {
		return fantasy.NewTextErrorResponse("Background agent lookup is not available"), false
	}
	status, content, childSessionID, found := lookup(agentID)
	if !found {
		return fantasy.ToolResponse{}, false
	}
	if status == "running" {
		return fantasy.NewTextResponse(fmt.Sprintf("Background agent %q is still running. Try again later.", agentID)), true
	}

	if childSessionID != "" && messages != nil {
		msgs, err := messages.List(ctx, childSessionID)
		if err == nil {
			if text, ok := latestAssistantText(msgs); ok {
				content = text
			}
		}
	}

	result := paginateSubtaskResult(content, offset, limit)
	return fantasy.NewTextResponse(fmt.Sprintf("Agent %q (%s):\n\n%s", agentID, status, result)), true
}

func latestAssistantText(msgs []message.Message) (string, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant || msg.IsSummaryMessage {
			continue
		}
		text := strings.TrimSpace(msg.Content().Text)
		if text == "" {
			continue
		}
		return text, true
	}
	return "", false
}

func latestSessionFallbackText(msgs []message.Message) (string, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		toolResults := msgs[i].ToolResults()
		for j := len(toolResults) - 1; j >= 0; j-- {
			result := toolResults[j]
			if finish, ok := result.SubagentFinish(); ok {
				if text := formatSubagentFinish(finish); text != "" {
					return text, true
				}
			}
			if text := strings.TrimSpace(result.Content); text != "" {
				return text, true
			}
		}
	}
	return "", false
}

func formatSubagentFinish(finish message.ToolResultSubagentFinish) string {
	var b strings.Builder
	if status := strings.TrimSpace(string(finish.Status)); status != "" {
		fmt.Fprintf(&b, "Status: %s\n", status)
	}
	if summary := strings.TrimSpace(finish.Summary); summary != "" {
		fmt.Fprintf(&b, "Summary: %s\n", summary)
	}
	if errText := strings.TrimSpace(finish.Error); errText != "" {
		fmt.Fprintf(&b, "Error: %s\n", errText)
	}
	writeSubagentFinishList(&b, "Artifacts", finish.Artifacts)
	writeSubagentFinishList(&b, "Files touched", finish.FilesTouched)
	writeSubagentFinishList(&b, "Patch plan", finish.PatchPlan)
	writeSubagentFinishList(&b, "Test results", finish.TestResults)
	writeSubagentFinishList(&b, "Followups", finish.Followups)
	writeSubagentFinishList(&b, "Risks", finish.Risks)
	writeSubagentFinishList(&b, "Next actions", finish.NextActions)
	if confidence := strings.TrimSpace(finish.Confidence); confidence != "" {
		fmt.Fprintf(&b, "Confidence: %s\n", confidence)
	}
	return strings.TrimSpace(b.String())
}

func writeSubagentFinishList(b *strings.Builder, label string, values []string) {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			filtered = append(filtered, trimmed)
		}
	}
	if len(filtered) == 0 {
		return
	}
	fmt.Fprintf(b, "%s:\n", label)
	for _, value := range filtered {
		fmt.Fprintf(b, "- %s\n", value)
	}
}

func formatSubtaskSessionText(sessionID, taskRef, text string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session: %s\n", sessionID)
	if taskRef = normalizeSubtaskTaskRef(taskRef); taskRef != "" {
		fmt.Fprintf(&b, "Task ref: %s\n", taskRef)
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(text))
	return b.String()
}

func formatSubtaskPreview(target subtaskResultTarget) string {
	var b strings.Builder
	if taskRef := normalizeSubtaskTaskRef(target.TaskRef); taskRef != "" {
		fmt.Fprintf(&b, "Task ref: %s\n", taskRef)
	}
	if status := strings.TrimSpace(string(target.Status)); status != "" {
		fmt.Fprintf(&b, "Status: %s\n", status)
	}
	if target.HasFullOutput {
		b.WriteString("Preview only; full output was not available from a child session.\n")
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(strings.TrimSpace(target.Preview))
	return strings.TrimSpace(b.String())
}

func paginateSubtaskResult(content string, offset, limit int) string {
	runes := []rune(content)
	if offset < 0 {
		offset = 0
	}
	if offset > len(runes) {
		offset = len(runes)
	}
	end := offset + limit
	if end > len(runes) {
		end = len(runes)
	}
	truncated := offset > 0 || end < len(runes)
	result := string(runes[offset:end])
	if truncated {
		omitted := len(runes) - end
		result = fmt.Sprintf("%s\n\n[Output truncated: showing characters %d-%d of %d. %d characters omitted. Use offset/limit to paginate.]", result, offset, end, len(runes), omitted)
	}
	return result
}

func isUnresolvedSubtaskSessionPlaceholder(sessionID string) bool {
	normalized := strings.ToLower(strings.TrimSpace(sessionID))
	if normalized == "messageid$$toolcallid" {
		return true
	}
	return strings.Contains(normalized, "messageid") && strings.Contains(normalized, "toolcallid") && strings.Contains(normalized, "$$")
}

func normalizeSubtaskTaskRef(taskRef string) string {
	taskRef = strings.TrimSpace(taskRef)
	taskRef = strings.TrimPrefix(taskRef, "subtask://")
	return strings.TrimSpace(taskRef)
}

func inferLatestChildSessionID(ctx context.Context, messages message.Service) (string, bool) {
	target, ok := inferLatestSubtaskTarget(ctx, messages)
	return target.SessionID, ok && strings.TrimSpace(target.SessionID) != ""
}

func inferLatestSubtaskTarget(ctx context.Context, messages message.Service) (subtaskResultTarget, bool) {
	if messages == nil {
		return subtaskResultTarget{}, false
	}

	currentSessionID := strings.TrimSpace(GetSessionFromContext(ctx))
	if currentSessionID == "" {
		return subtaskResultTarget{}, false
	}

	msgs, err := messages.List(ctx, currentSessionID)
	if err != nil {
		return subtaskResultTarget{}, false
	}

	for i := len(msgs) - 1; i >= 0; i-- {
		toolResults := msgs[i].ToolResults()
		for j := len(toolResults) - 1; j >= 0; j-- {
			if reducer, ok := toolResults[j].Reducer(); ok {
				for k := len(reducer.ChildSessions) - 1; k >= 0; k-- {
					if target, ok := targetFromReducerChild(reducer.ChildSessions[k]); ok {
						return target, true
					}
				}
			}
			subtask, ok := toolResults[j].SubtaskResult()
			if !ok {
				continue
			}
			childSessionID := strings.TrimSpace(subtask.ChildSessionID)
			if childSessionID != "" {
				return subtaskResultTarget{
					SessionID: childSessionID,
					TaskRef:   subtask.TaskRef,
					Status:    subtask.Status,
				}, true
			}
		}
	}

	return subtaskResultTarget{}, false
}

func resolveSubtaskTargetByTaskRef(ctx context.Context, messages message.Service, taskRef string) (subtaskResultTarget, bool) {
	taskRef = normalizeSubtaskTaskRef(taskRef)
	if messages == nil || taskRef == "" {
		return subtaskResultTarget{}, false
	}
	currentSessionID := strings.TrimSpace(GetSessionFromContext(ctx))
	if currentSessionID == "" {
		return subtaskResultTarget{}, false
	}
	msgs, err := messages.List(ctx, currentSessionID)
	if err != nil {
		return subtaskResultTarget{}, false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		toolResults := msgs[i].ToolResults()
		for j := len(toolResults) - 1; j >= 0; j-- {
			if reducer, ok := toolResults[j].Reducer(); ok {
				for k := len(reducer.ChildSessions) - 1; k >= 0; k-- {
					child := reducer.ChildSessions[k]
					if !subtaskRefMatches(taskRef, child.TaskRef, child.TaskID) {
						continue
					}
					return targetFromReducerChild(child)
				}
			}
			if subtask, ok := toolResults[j].SubtaskResult(); ok && subtaskRefMatches(taskRef, subtask.TaskRef) {
				return subtaskResultTarget{
					SessionID: strings.TrimSpace(subtask.ChildSessionID),
					TaskRef:   subtask.TaskRef,
					Status:    subtask.Status,
				}, true
			}
		}
	}
	return subtaskResultTarget{}, false
}

func targetFromReducerChild(child message.ToolResultReducerChildSession) (subtaskResultTarget, bool) {
	if strings.TrimSpace(child.SessionID) == "" && strings.TrimSpace(child.Preview) == "" {
		return subtaskResultTarget{}, false
	}
	return subtaskResultTarget{
		SessionID:     strings.TrimSpace(child.SessionID),
		TaskRef:       child.TaskRef,
		Preview:       child.Preview,
		Status:        child.Status,
		HasFullOutput: child.HasFullOutput,
	}, true
}

func subtaskRefMatches(target string, candidates ...string) bool {
	target = normalizeSubtaskTaskRef(target)
	if target == "" {
		return false
	}
	for _, candidate := range candidates {
		candidate = normalizeSubtaskTaskRef(candidate)
		if candidate == "" {
			continue
		}
		if strings.EqualFold(target, candidate) {
			return true
		}
	}
	return false
}
