package sessionevent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

const (
	SnapshotMessageLimit  = 20
	SnapshotPreviewBytes  = 512
	SnapshotResourceLimit = 50
	SnapshotDraftBytes    = 64 * 1024
)

// SnapshotSessionReader is the bounded metadata dependency for snapshots.
type SnapshotSessionReader interface {
	Get(context.Context, string) (session.Session, error)
}

// SnapshotMessageReader loads only a bounded tail page. Implementations must
// not satisfy ListPage by first loading all session messages.
type SnapshotMessageReader interface {
	ListRecent(context.Context, string, int) ([]message.Message, error)
}

// SnapshotRuntimeSource provides bounded in-memory state. Later services may
// enrich the resource summary slots without changing snapshot persistence IO.
type SnapshotRuntimeSource interface {
	SnapshotRuntime(string) RuntimeSnapshot
}

// SnapshotService builds a bounded first-screen projection.
type SnapshotService struct {
	sessions SnapshotSessionReader
	messages SnapshotMessageReader
	runtime  SnapshotRuntimeSource
	events   *Hub
}

type Snapshot struct {
	Session         SessionSummary    `json:"session"`
	Status          string            `json:"status"`
	ActiveTurn      *TurnSummary      `json:"activeTurn,omitempty"`
	Queue           QueueSummary      `json:"queue"`
	EffectiveConfig InferenceConfig   `json:"effectiveConfig"`
	MCPServers      []ResourceSummary `json:"mcpServers"`
	Terminals       []ResourceSummary `json:"terminals"`
	Messages        []MessageSummary  `json:"messages"`
	LatestSequence  uint64            `json:"latestSequence"`
	SessionRevision uint64            `json:"sessionRevision"`
}

type SessionSummary struct {
	ID                string `json:"id"`
	ParentSessionID   string `json:"parentSessionId,omitempty"`
	Kind              string `json:"kind"`
	Title             string `json:"title"`
	WorkspaceCWD      string `json:"workspaceCwd,omitempty"`
	CollaborationMode string `json:"collaborationMode"`
	PermissionMode    string `json:"permissionMode"`
	MessageCount      int64  `json:"messageCount"`
	PromptTokens      int64  `json:"promptTokens"`
	CompletionTokens  int64  `json:"completionTokens"`
	Archived          bool   `json:"archived"`
	Pinned            bool   `json:"pinned"`
	CreatedAt         int64  `json:"createdAt"`
	UpdatedAt         int64  `json:"updatedAt"`
}

type RuntimeSnapshot struct {
	Busy          bool
	QueueCount    int
	QueuePaused   bool
	Model         string
	Provider      string
	Inference     session.EffectiveInference
	MCPServers    []ResourceSummary
	Terminals     []ResourceSummary
	ActiveTurnID  string
	ActiveMessage string
}

type TurnSummary struct {
	ID        string           `json:"id,omitempty"`
	MessageID string           `json:"messageId,omitempty"`
	State     string           `json:"state"`
	Draft     *ActiveTurnDraft `json:"draft,omitempty"`
}

// ActiveTurnDraft is the bounded in-memory assistant text required to restore
// an active turn after a sequenced snapshot.
type ActiveTurnDraft struct {
	Text             string `json:"text"`
	Truncated        bool   `json:"truncated"`
	CapturedSequence uint64 `json:"capturedSequence"`
}

type QueueSummary struct {
	Count  int  `json:"count"`
	Paused bool `json:"paused"`
}

type InferenceConfig struct {
	Model            string   `json:"model,omitempty"`
	Provider         string   `json:"provider,omitempty"`
	MaxOutputTokens  *int64   `json:"maxOutputTokens,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"topP,omitempty"`
	TopK             *int64   `json:"topK,omitempty"`
	FrequencyPenalty *float64 `json:"frequencyPenalty,omitempty"`
	PresencePenalty  *float64 `json:"presencePenalty,omitempty"`
	Think            *bool    `json:"think,omitempty"`
	Revision         uint64   `json:"revision"`
}

type ResourceSummary struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"`
}

type MessageSummary struct {
	ID               string `json:"id"`
	Role             string `json:"role"`
	Preview          string `json:"preview,omitempty"`
	PreviewTruncated bool   `json:"previewTruncated,omitempty"`
	Model            string `json:"model,omitempty"`
	Provider         string `json:"provider,omitempty"`
	FinishReason     string `json:"finishReason,omitempty"`
	ToolCallCount    int    `json:"toolCallCount,omitempty"`
	// ToolCalls are bounded public lifecycle projections (id/name/status only).
	// Input/result bodies are never included — use live tool.* events for those.
	ToolCalls       []ToolCallSummary `json:"toolCalls,omitempty"`
	AttachmentCount int               `json:"attachmentCount,omitempty"`
	CreatedAt       int64             `json:"createdAt"`
	UpdatedAt       int64             `json:"updatedAt"`
}

// ToolCallSummary is the snapshot/history public tool identity used by desktop
// clients to hydrate ActivityShell without a second messages round-trip.
type ToolCallSummary struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Finished       bool   `json:"finished"`
	Status         string `json:"status,omitempty"`
	IsError        bool   `json:"isError,omitempty"`
	ChildSessionID string `json:"childSessionId,omitempty"`
}

func NewSnapshotService(
	sessions SnapshotSessionReader,
	messages SnapshotMessageReader,
	runtime SnapshotRuntimeSource,
	events *Hub,
) *SnapshotService {
	return &SnapshotService{sessions: sessions, messages: messages, runtime: runtime, events: events}
}

// Snapshot performs one session query and one indexed, bounded message-tail
// query regardless of total history size.
func (s *SnapshotService) Snapshot(ctx context.Context, sessionID string) (Snapshot, error) {
	if s == nil || s.sessions == nil || s.messages == nil {
		return Snapshot{}, errors.New("session snapshot source is unavailable")
	}
	sess, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	messages, err := s.messages.ListRecent(ctx, sessionID, SnapshotMessageLimit)
	if err != nil {
		return Snapshot{}, fmt.Errorf("loading snapshot messages: %w", err)
	}
	if len(messages) > SnapshotMessageLimit {
		return Snapshot{}, errors.New("snapshot message source exceeded its requested bound")
	}

	runtime := RuntimeSnapshot{}
	if s.runtime != nil {
		runtime = s.runtime.SnapshotRuntime(sessionID)
	}
	status := "idle"
	var activeTurn *TurnSummary
	if runtime.Busy {
		status = "running"
		activeTurn = &TurnSummary{ID: runtime.ActiveTurnID, MessageID: runtime.ActiveMessage, State: "running"}
	} else if runtime.QueueCount > 0 {
		status = "queued"
	}

	cut := SnapshotCut{}
	if s.events != nil {
		cut = s.events.SnapshotCut(sessionID)
	}
	if activeTurn != nil {
		if activeTurn.MessageID == "" {
			activeTurn.MessageID = cut.ActiveDraft.MessageID
		}
		if cut.ActiveDraft.Available {
			activeTurn.Draft = &ActiveTurnDraft{
				Text:             cut.ActiveDraft.Text,
				Truncated:        cut.ActiveDraft.Truncated,
				CapturedSequence: cut.LatestSequence,
			}
		}
	}
	effectiveConfig := InferenceConfig{Model: runtime.Model, Provider: runtime.Provider}
	if runtime.Inference.Model != "" {
		effectiveConfig = inferenceConfig(runtime.Inference)
	}
	result := Snapshot{
		Session: SessionSummary{
			ID:                sess.ID,
			ParentSessionID:   sess.ParentSessionID,
			Kind:              string(sess.Kind),
			Title:             sess.Title,
			WorkspaceCWD:      sess.WorkspaceCWD,
			CollaborationMode: string(sess.CollaborationMode),
			PermissionMode:    string(sess.PermissionMode),
			MessageCount:      sess.MessageCount,
			PromptTokens:      sess.PromptTokens,
			CompletionTokens:  sess.CompletionTokens,
			Archived:          sess.Archived,
			Pinned:            sess.Pinned,
			CreatedAt:         UnixSecondsToMilliseconds(sess.CreatedAt),
			UpdatedAt:         UnixSecondsToMilliseconds(sess.UpdatedAt),
		},
		Status:          status,
		ActiveTurn:      activeTurn,
		Queue:           QueueSummary{Count: runtime.QueueCount, Paused: runtime.QueuePaused},
		EffectiveConfig: effectiveConfig,
		MCPServers:      boundedResources(runtime.MCPServers),
		Terminals:       boundedResources(runtime.Terminals),
		Messages:        make([]MessageSummary, len(messages)),
		LatestSequence:  cut.LatestSequence,
		SessionRevision: cut.SessionRevision,
	}
	resultsByCall := snapshotToolResultsByCallID(messages)
	for index := range messages {
		result.Messages[index] = summarizeMessage(&messages[index], resultsByCall)
	}
	return result, nil
}

func inferenceConfig(value session.EffectiveInference) InferenceConfig {
	return InferenceConfig{
		Model: value.Model, Provider: value.Provider, MaxOutputTokens: value.MaxOutputTokens,
		Temperature: value.Temperature, TopP: value.TopP, TopK: value.TopK,
		FrequencyPenalty: value.FrequencyPenalty, PresencePenalty: value.PresencePenalty,
		Think: value.Think, Revision: value.Revision,
	}
}

func summarizeMessage(msg *message.Message, resultsByCall map[string]message.ToolResult) MessageSummary {
	preview, truncated := truncateUTF8(msg.Content().Text, SnapshotPreviewBytes)
	finishReason := ""
	if finish := msg.FinishPart(); finish != nil {
		finishReason = string(finish.Reason)
	}
	calls := msg.ToolCalls()
	localResults := snapshotToolResultsByCallID([]message.Message{*msg})
	for id, result := range resultsByCall {
		if _, exists := localResults[id]; !exists {
			localResults[id] = result
		}
	}
	return MessageSummary{
		ID:               msg.ID,
		Role:             string(msg.Role),
		Preview:          preview,
		PreviewTruncated: truncated,
		Model:            msg.Model,
		Provider:         msg.Provider,
		FinishReason:     finishReason,
		ToolCallCount:    len(calls),
		ToolCalls:        projectSnapshotToolCalls(msg.ID, calls, localResults),
		AttachmentCount:  len(msg.ImageURLContent()) + len(msg.BinaryContent()),
		CreatedAt:        UnixSecondsToMilliseconds(msg.CreatedAt),
		UpdatedAt:        UnixSecondsToMilliseconds(msg.UpdatedAt),
	}
}

func snapshotToolResultsByCallID(items []message.Message) map[string]message.ToolResult {
	out := make(map[string]message.ToolResult)
	for index := range items {
		for _, result := range items[index].ToolResults() {
			if result.ToolCallID == "" {
				continue
			}
			out[result.ToolCallID] = result
		}
	}
	return out
}

func projectSnapshotToolCalls(messageID string, calls []message.ToolCall, resultsByCall map[string]message.ToolResult) []ToolCallSummary {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCallSummary, len(calls))
	for index, call := range calls {
		meta := ToolCallSummary{
			ID:       call.ID,
			Name:     call.Name,
			Finished: call.Finished,
			Status:   "ready",
		}
		if result, ok := resultsByCall[call.ID]; ok {
			meta.Finished = true
			meta.IsError = result.IsError
			if result.IsError {
				meta.Status = "failed"
			} else {
				meta.Status = "completed"
			}
		} else if call.Finished {
			meta.Status = "completed"
		}
		if isAgentLikeToolName(call.Name) && messageID != "" && call.ID != "" {
			meta.ChildSessionID = messageID + "$$" + call.ID
		}
		out[index] = meta
	}
	return out
}

func isAgentLikeToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "agent", "agentic_fetch":
		return true
	default:
		return false
	}
}

// UnixSecondsToMilliseconds adapts persisted Unix-second values at the
// private protocol DTO boundary. Persistence intentionally remains in seconds.
func UnixSecondsToMilliseconds(seconds int64) int64 {
	return seconds * 1000
}

func boundedResources(resources []ResourceSummary) []ResourceSummary {
	limit := min(len(resources), SnapshotResourceLimit)
	result := make([]ResourceSummary, limit)
	copy(result, resources[:limit])
	return result
}

func truncateUTF8(value string, maxBytes int) (string, bool) {
	if len(value) <= maxBytes {
		return value, false
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end], true
}
