package sessionevent

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

const (
	SnapshotMessageLimit  = 20
	SnapshotPreviewBytes  = 512
	SnapshotResourceLimit = 50
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
	ID        string `json:"id,omitempty"`
	MessageID string `json:"messageId,omitempty"`
	State     string `json:"state"`
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
	AttachmentCount  int    `json:"attachmentCount,omitempty"`
	CreatedAt        int64  `json:"createdAt"`
	UpdatedAt        int64  `json:"updatedAt"`
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

	latestSequence := uint64(0)
	revision := uint64(0)
	if s.events != nil {
		latestSequence = s.events.LatestSequence(sessionID)
		revision = s.events.LatestRevision(sessionID)
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
			CreatedAt:         sess.CreatedAt,
			UpdatedAt:         sess.UpdatedAt,
		},
		Status:          status,
		ActiveTurn:      activeTurn,
		Queue:           QueueSummary{Count: runtime.QueueCount, Paused: runtime.QueuePaused},
		EffectiveConfig: effectiveConfig,
		MCPServers:      boundedResources(runtime.MCPServers),
		Terminals:       boundedResources(runtime.Terminals),
		Messages:        make([]MessageSummary, len(messages)),
		LatestSequence:  latestSequence,
		SessionRevision: revision,
	}
	for index := range messages {
		result.Messages[index] = summarizeMessage(&messages[index])
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

func summarizeMessage(msg *message.Message) MessageSummary {
	preview, truncated := truncateUTF8(msg.Content().Text, SnapshotPreviewBytes)
	finishReason := ""
	if finish := msg.FinishPart(); finish != nil {
		finishReason = string(finish.Reason)
	}
	return MessageSummary{
		ID:               msg.ID,
		Role:             string(msg.Role),
		Preview:          preview,
		PreviewTruncated: truncated,
		Model:            msg.Model,
		Provider:         msg.Provider,
		FinishReason:     finishReason,
		ToolCallCount:    len(msg.ToolCalls()),
		AttachmentCount:  len(msg.ImageURLContent()) + len(msg.BinaryContent()),
		CreatedAt:        msg.CreatedAt,
		UpdatedAt:        msg.UpdatedAt,
	}
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
