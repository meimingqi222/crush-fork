package message

import (
	"encoding/json"

	"charm.land/fantasy"
)

const fantasyMessageMetadataType = "crush.message_metadata"

func init() {
	fantasy.RegisterProviderType(fantasyMessageMetadataType, func(data []byte) (fantasy.ProviderOptionsData, error) {
		var metadata fantasyMessageMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return nil, err
		}
		return &metadata, nil
	})
}

type fantasyMessageMetadata struct {
	ID               string `json:"id,omitempty"`
	SessionID        string `json:"session_id,omitempty"`
	Model            string `json:"model,omitempty"`
	Provider         string `json:"provider,omitempty"`
	CreatedAt        int64  `json:"created_at,omitempty"`
	UpdatedAt        int64  `json:"updated_at,omitempty"`
	IsSummaryMessage bool   `json:"is_summary_message,omitempty"`
}

func newFantasyMessageMetadata(msg Message) *fantasyMessageMetadata {
	if msg.ID == "" &&
		msg.SessionID == "" &&
		msg.Model == "" &&
		msg.Provider == "" &&
		msg.CreatedAt == 0 &&
		msg.UpdatedAt == 0 &&
		!msg.IsSummaryMessage {
		return nil
	}

	return &fantasyMessageMetadata{
		ID:               msg.ID,
		SessionID:        msg.SessionID,
		Model:            msg.Model,
		Provider:         msg.Provider,
		CreatedAt:        msg.CreatedAt,
		UpdatedAt:        msg.UpdatedAt,
		IsSummaryMessage: msg.IsSummaryMessage,
	}
}

func (m *fantasyMessageMetadata) Options() {}

func (m fantasyMessageMetadata) MarshalJSON() ([]byte, error) {
	type plain fantasyMessageMetadata
	return fantasy.MarshalProviderType(fantasyMessageMetadataType, plain(m))
}

func (m *fantasyMessageMetadata) UnmarshalJSON(data []byte) error {
	type plain fantasyMessageMetadata
	var p plain
	if err := fantasy.UnmarshalProviderType(data, &p); err != nil {
		return err
	}
	*m = fantasyMessageMetadata(p)
	return nil
}

func applyFantasyMessageMetadata(msg *Message, metadata *fantasyMessageMetadata) {
	if metadata == nil || msg == nil {
		return
	}

	msg.ID = metadata.ID
	msg.SessionID = metadata.SessionID
	msg.Model = metadata.Model
	msg.Provider = metadata.Provider
	msg.CreatedAt = metadata.CreatedAt
	msg.UpdatedAt = metadata.UpdatedAt
	msg.IsSummaryMessage = metadata.IsSummaryMessage
}

func fantasyMessageMetadataFromMessage(msg fantasy.Message) (*fantasyMessageMetadata, bool) {
	if metadata, ok := fantasyMessageMetadataFromOptions(msg.ProviderOptions); ok {
		return metadata, true
	}

	for _, part := range msg.Content {
		if metadata, ok := fantasyMessageMetadataFromOptions(part.Options()); ok {
			return metadata, true
		}
	}

	return nil, false
}

func fantasyMessageMetadataFromOptions(opts fantasy.ProviderOptions) (*fantasyMessageMetadata, bool) {
	if opts == nil {
		return nil, false
	}

	metadata, ok := opts[fantasyMessageMetadataType].(*fantasyMessageMetadata)
	if !ok || metadata == nil {
		return nil, false
	}
	return metadata, true
}

func attachFantasyMessageMetadata(parts []fantasy.MessagePart, metadata *fantasyMessageMetadata) []fantasy.MessagePart {
	if metadata == nil || len(parts) == 0 {
		return parts
	}

	withMetadata := make([]fantasy.MessagePart, 0, len(parts))
	for _, part := range parts {
		switch p := part.(type) {
		case fantasy.TextPart:
			p.ProviderOptions = mergeFantasyProviderOptions(p.ProviderOptions, metadata)
			withMetadata = append(withMetadata, p)
		case fantasy.ReasoningPart:
			p.ProviderOptions = mergeFantasyProviderOptions(p.ProviderOptions, metadata)
			withMetadata = append(withMetadata, p)
		case fantasy.FilePart:
			p.ProviderOptions = mergeFantasyProviderOptions(p.ProviderOptions, metadata)
			withMetadata = append(withMetadata, p)
		case fantasy.ToolCallPart:
			p.ProviderOptions = mergeFantasyProviderOptions(p.ProviderOptions, metadata)
			withMetadata = append(withMetadata, p)
		case fantasy.ToolResultPart:
			p.ProviderOptions = mergeFantasyProviderOptions(p.ProviderOptions, metadata)
			withMetadata = append(withMetadata, p)
		default:
			withMetadata = append(withMetadata, part)
		}
	}

	return withMetadata
}

func mergeFantasyProviderOptions(opts fantasy.ProviderOptions, metadata *fantasyMessageMetadata) fantasy.ProviderOptions {
	if metadata == nil {
		return opts
	}

	merged := make(fantasy.ProviderOptions, len(opts)+1)
	for key, value := range opts {
		merged[key] = value
	}
	merged[fantasyMessageMetadataType] = metadata
	return merged
}
