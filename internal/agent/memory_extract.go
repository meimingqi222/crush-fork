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
	"github.com/charmbracelet/crush/internal/version"
)

var memoryUserAgent = fmt.Sprintf("Charm-Crush/%s (https://charm.land/crush)", version.Version)

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

const memoryConsolidationPrompt = `You are a long-term memory consolidation agent. Merge episodic memory events into durable semantic memories.

Rules:
- Keep only stable knowledge that should survive across sessions.
- Merge duplicates and prefer concise, self-contained memories.
- Do not preserve working memory, transient task progress, logs, or temporary files.
- Use scope "project" for repository/project knowledge, "user" for user preferences, and "global" only for broadly reusable facts.
- If a new memory replaces an existing one, set supersedes to the existing event ID.

Return a JSON array of objects with this exact shape:
[{"kind":"decision|preference|procedure|pitfall|reference","scope":"project|user|global","content":"Detailed durable memory","summary":"Brief summary","confidence":0.8,"importance":0.6,"tags":["relevant","tags"],"supersedes":"optional-existing-id"}]

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

func parseConsolidatedEvents(content string) ([]engine.ConsolidatedEvent, error) {
	content = strings.TrimSpace(content)
	start := strings.Index(content, "[")
	end := strings.LastIndex(content, "]")
	if start == -1 || end == -1 || end <= start {
		return nil, nil
	}

	jsonArray := content[start : end+1]
	var events []engine.ConsolidatedEvent
	if err := json.Unmarshal([]byte(jsonArray), &events); err != nil {
		return nil, nil
	}

	result := make([]engine.ConsolidatedEvent, 0, len(events))
	for _, e := range events {
		if e.Kind == "" || e.Content == "" {
			continue
		}
		if e.Scope == "" {
			e.Scope = engine.MemoryScopeProject
		}
		if e.Scope == engine.MemoryScopeSession || e.Kind == engine.MemoryKindTaskState || e.Kind == engine.MemoryKindWorkingMemory {
			continue
		}
		if e.Confidence <= 0 {
			e.Confidence = 0.7
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

func (c *coordinator) consolidateEventsWithModel(ctx context.Context, bgModel *backgroundModel, episodes, existing string) ([]engine.ConsolidatedEvent, error) {
	if bgModel == nil || strings.TrimSpace(episodes) == "" {
		return nil, nil
	}

	agent := fantasy.NewAgent(
		bgModel.model.Model,
		fantasy.WithSystemPrompt(memoryConsolidationPrompt),
		fantasy.WithMaxOutputTokens(2048),
		fantasy.WithUserAgent(memoryUserAgent),
	)

	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	prompt := "Episodic events:\n\n" + episodes
	if strings.TrimSpace(existing) != "" {
		prompt += "\n\nExisting consolidated memories:\n\n" + existing
	}

	resp, err := agent.Generate(copilot.ContextWithInitiatorType(callCtx, copilot.InitiatorAgent), fantasy.AgentCall{
		Prompt:          prompt,
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
		return nil, fmt.Errorf("LLM consolidation failed: %w", err)
	}
	if resp == nil {
		return nil, nil
	}

	return parseConsolidatedEvents(resp.Response.Content.Text())
}

// wireMemoryExtractor creates and attaches the Extractor to the engine.
// Called during engine setup in SetMemoryEngine.
func (c *coordinator) wireMemoryExtractor(eng *engine.Engine) {
	bgModel := c.resolveBackgroundModel(context.Background())
	if bgModel == nil {
		slog.Warn("No background model available, memory extraction disabled")
		eng.SetDegraded(true, "background model unavailable")
		return
	}
	eng.SetDegraded(false, "")

	extractor := engine.NewLLMExtractor(
		func(ctx context.Context, sessionID string) (engine.Transcript, error) {
			return c.buildTranscript(ctx, sessionID)
		},
		func(ctx context.Context, transcript string) ([]engine.ExtractedEvent, error) {
			bgModel := c.resolveBackgroundModel(ctx)
			if bgModel == nil {
				return nil, nil
			}
			return c.extractEventsFromTranscript(ctx, bgModel, transcript)
		},
		func(ctx context.Context, sessionID string) []string {
			return c.collectSessionFiles(ctx, sessionID)
		},
	)
	eng.SetExtractor(extractor)
	slog.Debug("Memory extractor wired to engine")
}

func (c *coordinator) wireMemoryConsolidator(eng *engine.Engine) {
	bgModel := c.resolveBackgroundModel(context.Background())
	if bgModel == nil {
		slog.Warn("No background model available, memory consolidation disabled")
		eng.SetDegraded(true, "background model unavailable")
		return
	}

	consolidator := engine.NewLLMConsolidator(
		func(ctx context.Context) ([]engine.MemoryEvent, error) {
			events, err := eng.EventStore().Query(ctx, engine.EventFilter{Limit: 1000})
			if err != nil {
				return nil, err
			}
			existing := events[:0]
			for _, evt := range events {
				if evt.Scope == engine.MemoryScopeSession || evt.Kind == engine.MemoryKindWorkingMemory || evt.Kind == engine.MemoryKindTaskState {
					continue
				}
				existing = append(existing, evt)
			}
			return existing, nil
		},
		func(ctx context.Context, episodes, existing string) ([]engine.ConsolidatedEvent, error) {
			bgModel := c.resolveBackgroundModel(ctx)
			if bgModel == nil {
				return nil, nil
			}
			return c.consolidateEventsWithModel(ctx, bgModel, episodes, existing)
		},
	)
	eng.SetConsolidator(consolidator)
	slog.Debug("Memory consolidator wired to engine")
}
