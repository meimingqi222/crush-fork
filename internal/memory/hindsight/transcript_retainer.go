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
	// Tags follow oh-my-pi's per-project-tagged pattern:
	// - kind:transcript_window identifies the content type
	// - project:xxx (from retainTags) provides project-level filtering when scoping=per-project-tagged
	// - session_id is kept in metadata only, not as a tag, to avoid polluting recall
	item := RetainItem{
		Content:    content,
		Context:    "crush transcript",
		DocumentID: fmt.Sprintf("transcript:%s:%d", sessionID, turnCount),
		Tags: appendUniqueTags([]string{
			"kind:transcript_window",
		}, r.retainTags...),
		Metadata: map[string]string{
			"kind":       "transcript_window",
			"session_id": sessionID,
			"turn":       fmt.Sprint(turnCount),
		},
		Async: true,
	}
	return r.client.Retain(ctx, []RetainItem{item})
}
