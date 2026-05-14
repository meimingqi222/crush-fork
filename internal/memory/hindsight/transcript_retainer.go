package hindsight

import (
	"context"
	"fmt"
	"strings"
)

// TranscriptRetainer stores raw transcript windows in Hindsight via retain.
type TranscriptRetainer struct {
	client     *Client
	retainTags []string
}

// NewTranscriptRetainer creates a Hindsight transcript retainer.
func NewTranscriptRetainer(client *Client, opts ...MaterializerOption) *TranscriptRetainer {
	m := &Materializer{}
	for _, opt := range opts {
		opt(m)
	}
	return &TranscriptRetainer{
		client:     client,
		retainTags: append([]string(nil), m.retainTags...),
	}
}

// RetainTranscript implements engine.TranscriptRetainer.
func (r *TranscriptRetainer) RetainTranscript(ctx context.Context, sessionID string, turnCount int, content string) error {
	sessionID = strings.TrimSpace(sessionID)
	content = strings.TrimSpace(content)
	if r == nil || r.client == nil || sessionID == "" || content == "" {
		return nil
	}
	if turnCount < 0 {
		return fmt.Errorf("turn count must be non-negative")
	}
	item := RetainItem{
		Content:    content,
		Context:    "crush transcript",
		DocumentID: fmt.Sprintf("transcript:%s:%d", sessionID, turnCount),
		Tags: appendUniqueTags([]string{
			"scope:session",
			"kind:transcript_window",
			"session:" + sessionID,
		}, r.retainTags...),
		Metadata: map[string]string{
			"scope":      "session",
			"kind":       "transcript_window",
			"session_id": sessionID,
			"turn":       fmt.Sprint(turnCount),
		},
		Async: true,
	}
	return r.client.Retain(ctx, []RetainItem{item})
}
