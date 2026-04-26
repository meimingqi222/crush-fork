package agent

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
)

const (
	sessionMemoryMaxHistory                = 12
	sessionMemoryInitializationTokens      = 10_000
	sessionMemoryMinimumTokensBetweenTurns = 5_000
	sessionMemoryToolCallsBetweenUpdates   = 3
)

const sessionMemoryPrompt = `You maintain a single session memory entry that helps future turns quickly recover the current working state.

Return JSON with exactly one array entry using this shape:
[{"action":"update","key":"session/<session-id>/current","description":"Current session state","content":"...","type":"project","scope":"session"}]

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

func shouldUpdateSessionMemory(initialized bool, currentPromptTokens, tokensAtLastExtraction int64, toolCallsSinceLastExtraction, currentRunToolUses int) (bool, bool) {
	if currentPromptTokens <= 0 {
		return false, initialized
	}
	if !initialized {
		if currentPromptTokens < sessionMemoryInitializationTokens {
			return false, false
		}
		initialized = true
	}
	if max(0, currentPromptTokens-tokensAtLastExtraction) < sessionMemoryMinimumTokensBetweenTurns {
		return false, initialized
	}
	if toolCallsSinceLastExtraction >= sessionMemoryToolCallsBetweenUpdates {
		return true, initialized
	}
	return currentRunToolUses == 0, initialized
}

func updateSessionMemory(ctx context.Context, memorySvc memory.Service, bgModel *backgroundModel, tracker filetracker.Service, sessionID, prompt string, history []string) {
	if memorySvc == nil || bgModel == nil {
		return
	}

	historyBlock := buildSessionMemoryHistory(history)
	if historyBlock == "" {
		return
	}

	ctx = context.WithValue(ctx, sessionMemoryContextKey{}, sessionID)
	filesBlock := buildSessionMemoryFiles(ctx, tracker)

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

	agent := fantasy.NewAgent(
		bgModel.model.Model,
		fantasy.WithSystemPrompt(sessionMemoryPrompt),
		fantasy.WithMaxOutputTokens(1024),
		fantasy.WithUserAgent(memoryUserAgent),
	)

	updateCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, err := agent.Generate(copilot.ContextWithInitiatorType(updateCtx, copilot.InitiatorAgent), fantasy.AgentCall{
		Prompt:          promptBuilder.String(),
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
		slog.Warn("Session memory update failed", "error", err, "session_id", sessionID)
		return
	}
	if resp == nil {
		return
	}

	content := resp.Response.Content.Text()
	memories := parseExtractedMemories(content)
	if len(memories) == 0 {
		return
	}
	for i := range memories {
		memories[i].Key = sessionMemoryKey(sessionID)
		if memories[i].Description == "" {
			memories[i].Description = "Current session state"
		}
		memories[i].Scope = "session"
		if memories[i].Type == "" {
			memories[i].Type = "project"
		}
		if memories[i].Action == "" || memories[i].Action == string(memoryOperationStore) {
			memories[i].Action = string(memoryOperationUpdate)
		}
	}
	if err := applyExtractedMemories(ctx, memorySvc, memories, "session_id", sessionID, "source", "session_memory"); err != nil {
		slog.Warn("Failed to apply session memory", "error", err, "session_id", sessionID)
	}
}
