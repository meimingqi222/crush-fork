package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
)

const (
	sessionMemoryMaxHistory                = 12
	sessionMemoryInitializationTokens      = 10_000
	sessionMemoryMinimumTokensBetweenTurns = 5_000
	sessionMemoryToolCallsBetweenUpdates   = 3
	workingMemoryTTL                       = 24 * time.Hour
)

const sessionMemoryPrompt = `You maintain a single session memory entry that helps future turns quickly recover the current working state.

Return JSON with exactly one array entry using this shape:
[{"content":"..."}]

Rules:
- Summarize only the current durable session state that would help the next turn continue work.
- Include: current goal, important recent decisions, relevant files, open issues, and next likely step.
- Do NOT copy long transcripts, tool logs, or temporary noise.
- Keep the content concise and factual.
- If there is not enough useful state yet, return [].`

func sessionMemoryKey(sessionID string) string {
	return fmt.Sprintf("session/%s/current", strings.TrimSpace(sessionID))
}

func buildSessionMemoryHistory(history []string) string {
	if len(history) == 0 {
		return ""
	}
	if len(history) > sessionMemoryMaxHistory {
		history = history[len(history)-sessionMemoryMaxHistory:]
	}
	return strings.Join(history, "\n\n")
}

func buildSessionMemoryFiles(ctx context.Context, tracker filetracker.Service) string {
	if tracker == nil {
		return ""
	}
	sessionID, _ := ctx.Value(sessionMemoryContextKey{}).(string)
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	paths, err := tracker.ListReadFiles(ctx, sessionID)
	if err != nil || len(paths) == 0 {
		return ""
	}
	if len(paths) > 8 {
		paths = paths[len(paths)-8:]
	}
	lines := make([]string, 0, len(paths))
	for _, path := range paths {
		lines = append(lines, "- "+filepath.ToSlash(path))
	}
	return strings.Join(lines, "\n")
}

type sessionMemoryContextKey struct{}

type sessionMemoryUpdate struct {
	Content string `json:"content"`
}

func shouldUpdateSessionMemory(initialized bool, currentPromptTokens, tokensAtLastExtraction int64, toolCallsSinceLastExtraction, currentRunToolUses int) (bool, bool, int64) {
	if currentPromptTokens <= 0 {
		return false, initialized, tokensAtLastExtraction
	}
	if !initialized {
		if currentPromptTokens < sessionMemoryInitializationTokens {
			return false, false, tokensAtLastExtraction
		}
		initialized = true
		return true, initialized, currentPromptTokens
	}
	if currentPromptTokens < tokensAtLastExtraction {
		return false, initialized, currentPromptTokens
	}
	if currentPromptTokens-tokensAtLastExtraction < sessionMemoryMinimumTokensBetweenTurns {
		return false, initialized, tokensAtLastExtraction
	}
	if toolCallsSinceLastExtraction >= sessionMemoryToolCallsBetweenUpdates {
		return true, initialized, currentPromptTokens
	}
	if currentRunToolUses == 0 {
		return true, initialized, currentPromptTokens
	}
	return false, initialized, tokensAtLastExtraction
}

func buildSessionMemoryPrompt(sessionID, prompt, historyBlock, filesBlock string) string {
	var promptBuilder strings.Builder
	fmt.Fprintf(&promptBuilder, "Session ID: %s\n", sessionID)
	fmt.Fprintf(&promptBuilder, "Latest user prompt: %s\n\n", strings.TrimSpace(prompt))
	promptBuilder.WriteString("Recent conversation:\n")
	promptBuilder.WriteString(historyBlock)
	if filesBlock != "" {
		promptBuilder.WriteString("\n\nRecently read files:\n")
		promptBuilder.WriteString(filesBlock)
	}
	promptBuilder.WriteString("\n\nUpdate the single session memory entry for this session.")
	return promptBuilder.String()
}

func generateSessionMemory(ctx context.Context, bgModel *backgroundModel, prompt string) (string, error) {
	if bgModel == nil {
		return "", nil
	}

	agent := fantasy.NewAgent(
		bgModel.model.Model,
		fantasy.WithSystemPrompt(sessionMemoryPrompt),
		fantasy.WithMaxOutputTokens(1024),
		fantasy.WithUserAgent(memoryUserAgent),
	)

	updateCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, err := agent.Generate(copilot.ContextWithInitiatorType(updateCtx, copilot.InitiatorAgent), fantasy.AgentCall{
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
		return "", fmt.Errorf("session memory generation failed: %w", err)
	}
	if resp == nil {
		return "", nil
	}
	return resp.Response.Content.Text(), nil
}

func updateSessionMemoryEventStore(ctx context.Context, store engine.EventStore, bgModel *backgroundModel, tracker filetracker.Service, sessionID, prompt string, history []string) {
	if store == nil || bgModel == nil {
		return
	}

	historyBlock := buildSessionMemoryHistory(history)
	if historyBlock == "" {
		return
	}

	ctx = context.WithValue(ctx, sessionMemoryContextKey{}, sessionID)
	filesBlock := buildSessionMemoryFiles(ctx, tracker)

	memoryPrompt := buildSessionMemoryPrompt(sessionID, prompt, historyBlock, filesBlock)
	content, err := generateSessionMemory(ctx, bgModel, memoryPrompt)
	if err != nil {
		slog.Warn("Session memory update failed", "error", err, "session_id", sessionID)
		return
	}
	if content == "" {
		return
	}

	memories := parseSessionMemoryUpdates(content)
	if len(memories) == 0 {
		return
	}

	now := time.Now()
	expiresAt := now.Add(workingMemoryTTL)
	for i, mem := range memories {
		if mem.Content == "" {
			continue
		}

		eventID := fmt.Sprintf("wm-%s-%d-%d", sessionID, now.UnixNano(), i)
		event := engine.MemoryEvent{
			ID:      eventID,
			Scope:   engine.MemoryScopeSession,
			Kind:    engine.MemoryKindWorkingMemory,
			Content: mem.Content,
			Source: engine.MemorySourceRef{
				SessionID: sessionID,
				Files:     listFilesFromTracker(ctx, tracker, sessionID),
			},
			CreatedAt: now,
			UpdatedAt: now,
			ExpiresAt: &expiresAt,
			Tags:      []string{"working_memory"},
		}

		if err := store.Append(ctx, event); err != nil {
			slog.Warn("Failed to append working memory event", "error", err, "session_id", sessionID)
			return
		}
		slog.Debug("Working memory event stored", "session_id", sessionID, "event_id", eventID)
	}
}

func parseSessionMemoryUpdates(content string) []sessionMemoryUpdate {
	content = strings.TrimSpace(content)
	startIdx := strings.Index(content, "[")
	endIdx := strings.LastIndex(content, "]")
	if startIdx == -1 || endIdx == -1 || endIdx <= startIdx {
		return nil
	}

	var updates []sessionMemoryUpdate
	if err := json.Unmarshal([]byte(content[startIdx:endIdx+1]), &updates); err != nil {
		return nil
	}

	result := make([]sessionMemoryUpdate, 0, len(updates))
	for _, update := range updates {
		update.Content = strings.TrimSpace(update.Content)
		if update.Content != "" {
			result = append(result, update)
		}
	}
	return result
}

func readWorkingMemoryContent(ctx context.Context, store engine.EventStore, sessionID string) string {
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return ""
	}

	scope := engine.MemoryScopeSession
	kind := engine.MemoryKindWorkingMemory
	events, err := store.Query(ctx, engine.EventFilter{
		Scope:     &scope,
		Kind:      &kind,
		SessionID: &sessionID,
		Limit:     50,
	})
	if err != nil {
		slog.Warn("Failed to read working memory", "error", err, "session_id", sessionID)
		return ""
	}
	if len(events) == 0 {
		return ""
	}

	latest := events[len(events)-1]
	return latest.Content
}

func listFilesFromTracker(ctx context.Context, tracker filetracker.Service, sessionID string) []string {
	if tracker == nil {
		return nil
	}
	ctx = context.WithValue(ctx, sessionMemoryContextKey{}, sessionID)
	paths, err := tracker.ListReadFiles(ctx, sessionID)
	if err != nil || len(paths) == 0 {
		return nil
	}
	if len(paths) > 8 {
		paths = paths[len(paths)-8:]
	}
	result := make([]string, len(paths))
	for i, p := range paths {
		result[i] = filepath.ToSlash(p)
	}
	return result
}
