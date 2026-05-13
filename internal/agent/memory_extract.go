package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
)

const episodicExtractPrompt = `You are an episodic memory extraction agent. Analyze the conversation transcript and extract durable episodic memories.

Memory types:
- decision: Technical decisions, architecture choices, rejected alternatives.
- preference: User preferences, working styles, tooling choices.
- procedure: Step-by-step workflows, build steps, deployment procedures.
- pitfall: Mistakes, gotchas, anti-patterns, things to avoid.
- reference: Useful references, links, external resources, documentation.
- task_state: Task completion status, progress, blockers.

Rules:
- Extract only durable knowledge. Prefer stable decisions, project context, confirmed workflows.
- Do NOT save transient task state, one-off logs, or temporary file contents.
- Each event should be self-contained and meaningful on its own.
- scope: "session" for temporary context, "project" for lasting project knowledge, "user" for personal preferences.
- confidence: 0.0-1.0 indicating how sure you are this is correct.
- importance: 0.0-1.0 indicating how valuable this memory is for future work.

Return a JSON array of objects with this exact shape:
[{"kind":"decision|preference|procedure|pitfall|reference|task_state","scope":"session|project|user","content":"Detailed description","summary":"Brief summary","confidence":0.8,"importance":0.6,"tags":["relevant","tags"]}]

Return [] if nothing worth saving.`

// parseExtractedEvents parses a JSON array of ExtractedEvent from an LLM response.
func parseExtractedEvents(content string) ([]engine.ExtractedEvent, error) {
	content = strings.TrimSpace(content)
	start := strings.Index(content, "[")
	end := strings.LastIndex(content, "]")
	if start == -1 || end == -1 || end <= start {
		return nil, nil
	}

	jsonArray := content[start : end+1]
	var events []engine.ExtractedEvent
	if err := json.Unmarshal([]byte(jsonArray), &events); err != nil {
		return nil, nil
	}

	// Sanitize: filter empties, set defaults.
	result := make([]engine.ExtractedEvent, 0, len(events))
	for _, e := range events {
		if e.Kind == "" || e.Content == "" {
			continue
		}
		if e.Scope == "" {
			e.Scope = engine.MemoryScopeSession
		}
		if e.Confidence <= 0 {
			e.Confidence = 0.5
		}
		if e.Importance <= 0 {
			e.Importance = 0.5
		}
		result = append(result, e)
	}

	return result, nil
}

// buildTranscript retrieves a session's messages and formats them as a
// Transcript (text + message IDs) for memory extraction.
func (c *coordinator) buildTranscript(ctx context.Context, sessionID string) (engine.Transcript, error) {
	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		return engine.Transcript{}, fmt.Errorf("listing messages: %w", err)
	}

	var lines []string
	var msgIDs []string
	for _, msg := range msgs {
		msgIDs = append(msgIDs, msg.ID)
		switch msg.Role {
		case message.User:
			if text := strings.TrimSpace(msg.Content().Text); text != "" {
				lines = append(lines, "USER: "+text)
			}
		case message.Assistant:
			if text := strings.TrimSpace(msg.Content().Text); text != "" {
				lines = append(lines, "ASSISTANT: "+text)
			}
		}
		if len(strings.Join(lines, "\n")) >= 4000 {
			break
		}
	}

	return engine.Transcript{
		Text:       strings.Join(lines, "\n"),
		MessageIDs: msgIDs,
	}, nil
}

// collectSessionFiles returns the list of files read in a session.
func (c *coordinator) collectSessionFiles(ctx context.Context, sessionID string) []string {
	paths, err := c.filetracker.ListReadFiles(ctx, sessionID)
	if err != nil || len(paths) == 0 {
		return nil
	}
	if len(paths) > 8 {
		paths = paths[len(paths)-8:]
	}
	result := make([]string, len(paths))
	for i, p := range paths {
		result[i] = p
	}
	return result
}

// extractEventsFromTranscript calls the background LLM to analyze a session
// transcript and returns extracted episodic memory events.
func (c *coordinator) extractEventsFromTranscript(ctx context.Context, bgModel *backgroundModel, transcript string) ([]engine.ExtractedEvent, error) {
	if bgModel == nil {
		return nil, nil
	}
	if len(transcript) < 200 {
		return nil, nil
	}

	agent := fantasy.NewAgent(
		bgModel.model.Model,
		fantasy.WithSystemPrompt(episodicExtractPrompt),
		fantasy.WithMaxOutputTokens(2048),
		fantasy.WithUserAgent(memoryUserAgent),
	)

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := agent.Generate(copilot.ContextWithInitiatorType(callCtx, copilot.InitiatorAgent), fantasy.AgentCall{
		Prompt:          "Extract episodic memories from this conversation:\n\n" + transcript,
		ProviderOptions: getProviderOptions(bgModel.model, bgModel.provider),
		PrepareStep: func(callCtx context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			callCtx = copilot.ContextWithInitiatorType(callCtx, copilot.InitiatorAgent)
			prepared.Messages = options.Messages
			if bgModel.provider.SystemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(bgModel.provider.SystemPromptPrefix)}, prepared.Messages...)
			}
			return callCtx, prepared, nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("LLM extraction failed: %w", err)
	}
	if resp == nil {
		return nil, nil
	}

	return parseExtractedEvents(resp.Response.Content.Text())
}

// wireMemoryExtractor creates and attaches the Extractor to the engine.
// Called during engine setup in SetMemoryEngine.
func (c *coordinator) wireMemoryExtractor(eng *engine.Engine) {
	bgModel := c.resolveBackgroundModel(context.Background())
	if bgModel == nil {
		slog.Warn("No background model available, memory extraction disabled")
		return
	}

	extractor := engine.NewLLMExtractor(
		func(ctx context.Context, sessionID string) (engine.Transcript, error) {
			return c.buildTranscript(ctx, sessionID)
		},
		func(ctx context.Context, transcript string) ([]engine.ExtractedEvent, error) {
			return c.extractEventsFromTranscript(ctx, bgModel, transcript)
		},
		func(ctx context.Context, sessionID string) []string {
			return c.collectSessionFiles(ctx, sessionID)
		},
	)
	eng.SetExtractor(extractor)
	slog.Debug("Memory extractor wired to engine")
}
