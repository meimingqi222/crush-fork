package message

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
)

type CreateMessageParams struct {
	Role                   MessageRole
	Parts                  []ContentPart
	Model                  string
	Provider               string
	IsSummaryMessage       bool
	ActivatedDeferredTools []string
}

type PageCursor struct {
	CreatedAt int64
	ID        string
}

type Service interface {
	pubsub.Subscriber[Message]
	Create(ctx context.Context, sessionID string, params CreateMessageParams) (Message, error)
	Update(ctx context.Context, message Message) error
	Get(ctx context.Context, id string) (Message, error)
	GetRetrySource(ctx context.Context, sessionID, messageID string) (Message, error)
	List(ctx context.Context, sessionID string) ([]Message, error)
	ListPage(ctx context.Context, sessionID string, offset, limit int) ([]Message, error)
	ListRecent(ctx context.Context, sessionID string, limit int) ([]Message, error)
	ListBefore(ctx context.Context, sessionID string, before *PageCursor, limit int) ([]Message, error)
	Count(ctx context.Context, sessionID string) (int64, error)
	ListUserMessages(ctx context.Context, sessionID string) ([]Message, error)
	ListAllUserMessages(ctx context.Context) ([]Message, error)
	Delete(ctx context.Context, id string) error
	DeleteSessionMessages(ctx context.Context, sessionID string) error
}

type service struct {
	*pubsub.Broker[Message]
	q db.Querier
}

func NewService(q db.Querier) Service {
	return &service{
		Broker: pubsub.NewBroker[Message](),
		q:      q,
	}
}

func (s *service) Delete(ctx context.Context, id string) error {
	message, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	err = s.q.DeleteMessage(ctx, message.ID)
	if err != nil {
		return err
	}
	// Clone the message before publishing to avoid race conditions with
	// concurrent modifications to the Parts slice.
	s.Publish(pubsub.DeletedEvent, message.Clone())
	return nil
}

func (s *service) Create(ctx context.Context, sessionID string, params CreateMessageParams) (Message, error) {
	if params.Role != Assistant {
		params.Parts = append(params.Parts, Finish{
			Reason: "stop",
		})
	}
	activatedDeferredTools := normalizeDeferredToolNames(params.ActivatedDeferredTools)
	partsJSON, err := marshalParts(params.Parts, activatedDeferredTools)
	if err != nil {
		return Message{}, err
	}
	isSummary := int64(0)
	if params.IsSummaryMessage {
		isSummary = 1
	}
	dbMessage, err := s.q.CreateMessage(ctx, db.CreateMessageParams{
		ID:               uuid.New().String(),
		SessionID:        sessionID,
		Role:             string(params.Role),
		Parts:            string(partsJSON),
		Model:            sql.NullString{String: string(params.Model), Valid: true},
		Provider:         sql.NullString{String: params.Provider, Valid: params.Provider != ""},
		IsSummaryMessage: isSummary,
	})
	if err != nil {
		return Message{}, err
	}
	message, err := s.fromDBItem(dbMessage)
	if err != nil {
		return Message{}, err
	}
	message.ActivatedDeferredTools = activatedDeferredTools
	// Clone the message before publishing to avoid race conditions with
	// concurrent modifications to the Parts slice.
	s.Publish(pubsub.CreatedEvent, message.Clone())
	return message, nil
}

func (s *service) DeleteSessionMessages(ctx context.Context, sessionID string) error {
	messages, err := s.List(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, message := range messages {
		if message.SessionID == sessionID {
			err = s.Delete(ctx, message.ID)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *service) Update(ctx context.Context, message Message) error {
	message.ActivatedDeferredTools = normalizeDeferredToolNames(message.ActivatedDeferredTools)
	parts, err := marshalParts(message.Parts, message.ActivatedDeferredTools)
	if err != nil {
		return err
	}
	finishedAt := sql.NullInt64{}
	if f := message.FinishPart(); f != nil {
		finishedAt.Int64 = f.Time
		finishedAt.Valid = true
	}
	err = s.q.UpdateMessage(ctx, db.UpdateMessageParams{
		ID:               message.ID,
		Parts:            string(parts),
		FinishedAt:       finishedAt,
		InputTokens:      message.Usage.InputTokens,
		OutputTokens:     message.Usage.OutputTokens,
		ReasoningTokens:  message.Usage.ReasoningTokens,
		CacheReadTokens:  message.Usage.CacheReadTokens,
		CacheWriteTokens: message.Usage.CacheWriteTokens,
	})
	if err != nil {
		return err
	}
	message.UpdatedAt = time.Now().Unix()
	// Clone the message before publishing to avoid race conditions with
	// concurrent modifications to the Parts slice.
	s.Publish(pubsub.UpdatedEvent, message.Clone())
	return nil
}

func (s *service) Get(ctx context.Context, id string) (Message, error) {
	dbMessage, err := s.q.GetMessage(ctx, id)
	if err != nil {
		return Message{}, err
	}
	return s.fromDBItem(dbMessage)
}

func (s *service) GetRetrySource(ctx context.Context, sessionID, messageID string) (Message, error) {
	dbMessage, err := s.q.GetRetrySourceMessage(ctx, db.GetRetrySourceMessageParams{
		MessageID: messageID,
		SessionID: sessionID,
	})
	if err != nil {
		return Message{}, err
	}
	return s.fromDBItem(dbMessage)
}

func (s *service) List(ctx context.Context, sessionID string) ([]Message, error) {
	start := time.Now()
	dbMessages, err := s.q.ListMessagesBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	slog.Debug("[PERF] message.List: DB query done", "duration", time.Since(start), "session_id", sessionID, "count", len(dbMessages))
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	slog.Debug("[PERF] message.List: fromDBItem conversion done", "duration", time.Since(start), "session_id", sessionID)
	return messages, nil
}

func (s *service) ListPage(ctx context.Context, sessionID string, offset, limit int) ([]Message, error) {
	dbMessages, err := s.q.ListMessagesBySessionPage(ctx, db.ListMessagesBySessionPageParams{
		SessionID: sessionID,
		Limit:     int64(limit),
		Offset:    int64(offset),
	})
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

// ListRecent returns at most limit newest messages in chronological order.
// The SQL query reads the indexed tail directly and does not scan history via
// OFFSET.
func (s *service) ListRecent(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	dbMessages, err := s.q.ListRecentMessagesBySession(ctx, db.ListRecentMessagesBySessionParams{
		SessionID: sessionID,
		Limit:     int64(limit),
	})
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for index := range dbMessages {
		messages[len(dbMessages)-1-index], err = s.fromDBItem(dbMessages[index])
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

// ListBefore performs reverse-chronological keyset pagination. The composite
// created_at/id boundary remains valid when newer rows are inserted or earlier
// pages are deleted.
func (s *service) ListBefore(ctx context.Context, sessionID string, before *PageCursor, limit int) ([]Message, error) {
	params := db.ListMessagesBeforeParams{SessionID: sessionID, HasCursor: int64(0), Limit: int64(limit)}
	if before != nil {
		params.HasCursor = 1
		params.BeforeCreatedAt = before.CreatedAt
		params.BeforeID = before.ID
	}
	dbMessages, err := s.q.ListMessagesBefore(ctx, params)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for index := range dbMessages {
		messages[index], err = s.fromDBItem(dbMessages[index])
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) Count(ctx context.Context, sessionID string) (int64, error) {
	return s.q.CountMessagesBySession(ctx, sessionID)
}

func (s *service) ListUserMessages(ctx context.Context, sessionID string) ([]Message, error) {
	dbMessages, err := s.q.ListUserMessagesBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) ListAllUserMessages(ctx context.Context) ([]Message, error) {
	dbMessages, err := s.q.ListAllUserMessages(ctx)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, len(dbMessages))
	for i, dbMessage := range dbMessages {
		messages[i], err = s.fromDBItem(dbMessage)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *service) fromDBItem(item db.Message) (Message, error) {
	parts, activatedDeferredTools, err := unmarshalParts([]byte(item.Parts))
	if err != nil {
		return Message{}, err
	}
	msg := Message{
		ID:        item.ID,
		SessionID: item.SessionID,
		Role:      MessageRole(item.Role),
		Parts:     parts,
		Usage: Usage{
			InputTokens:      item.InputTokens,
			OutputTokens:     item.OutputTokens,
			ReasoningTokens:  item.ReasoningTokens,
			CacheReadTokens:  item.CacheReadTokens,
			CacheWriteTokens: item.CacheWriteTokens,
		},
		Model:            item.Model.String,
		Provider:         item.Provider.String,
		CreatedAt:        item.CreatedAt,
		UpdatedAt:        item.UpdatedAt,
		IsSummaryMessage: item.IsSummaryMessage != 0,
	}
	msg.ActivatedDeferredTools = activatedDeferredTools
	return msg, nil
}

type partType string

const (
	reasoningType  partType = "reasoning"
	textType       partType = "text"
	imageURLType   partType = "image_url"
	binaryType     partType = "binary"
	toolCallType   partType = "tool_call"
	toolResultType partType = "tool_result"
	finishType     partType = "finish"
)

type partWrapper struct {
	Type                   partType    `json:"type"`
	Data                   ContentPart `json:"data"`
	ActivatedDeferredTools []string    `json:"activated_deferred_tools,omitempty"`
}

func marshalParts(parts []ContentPart, activatedDeferredTools []string) ([]byte, error) {
	activatedDeferredTools = normalizeDeferredToolNames(activatedDeferredTools)
	wrappedParts := make([]partWrapper, len(parts))

	for i, part := range parts {
		var typ partType

		switch part.(type) {
		case ReasoningContent:
			typ = reasoningType
		case TextContent:
			typ = textType
		case ImageURLContent:
			typ = imageURLType
		case BinaryContent:
			typ = binaryType
		case ToolCall:
			typ = toolCallType
		case ToolResult:
			typ = toolResultType
		case Finish:
			typ = finishType
		default:
			return nil, fmt.Errorf("unknown part type: %T", part)
		}

		wrappedParts[i] = partWrapper{
			Type:                   typ,
			Data:                   part,
			ActivatedDeferredTools: activatedDeferredTools,
		}
	}
	return json.Marshal(wrappedParts)
}

func unmarshalParts(data []byte) ([]ContentPart, []string, error) {
	temp := []json.RawMessage{}

	if err := json.Unmarshal(data, &temp); err != nil {
		return nil, nil, err
	}

	parts := make([]ContentPart, 0)
	var activatedDeferredTools []string

	for _, rawPart := range temp {
		var wrapper struct {
			Type                   partType        `json:"type"`
			Data                   json.RawMessage `json:"data"`
			ActivatedDeferredTools []string        `json:"activated_deferred_tools,omitempty"`
		}

		if err := json.Unmarshal(rawPart, &wrapper); err != nil {
			return nil, nil, err
		}
		if len(activatedDeferredTools) == 0 {
			activatedDeferredTools = normalizeDeferredToolNames(wrapper.ActivatedDeferredTools)
		}

		switch wrapper.Type {
		case reasoningType:
			part := ReasoningContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, nil, err
			}
			parts = append(parts, part)
		case textType:
			part := TextContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, nil, err
			}
			parts = append(parts, part)
		case imageURLType:
			part := ImageURLContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, nil, err
			}
			parts = append(parts, part)
		case binaryType:
			part := BinaryContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, nil, err
			}
			parts = append(parts, part)
		case toolCallType:
			part := ToolCall{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, nil, err
			}
			parts = append(parts, part)
		case toolResultType:
			part := ToolResult{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, nil, err
			}
			parts = append(parts, part)
		case finishType:
			part := Finish{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, nil, err
			}
			parts = append(parts, part)
		default:
			return nil, nil, fmt.Errorf("unknown part type: %s", wrapper.Type)
		}
	}

	return parts, activatedDeferredTools, nil
}
