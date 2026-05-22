package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/charmbracelet/crush/internal/session"
)

//go:embed templates/handoff.md
var handoffPrompt []byte

var handoffCodeFenceRegex = regexp.MustCompile("(?s)^```(?:json)?\\s*(.*?)\\s*```$")

type HandoffDraft struct {
	Title         string
	Prompt        string
	RelevantFiles []string
}

type handoffResponse struct {
	Title         string   `json:"title"`
	Prompt        string   `json:"prompt"`
	RelevantFiles []string `json:"relevant_files"`
}

func (c *coordinator) GenerateHandoff(ctx context.Context, sourceSessionID, goal string) (HandoffDraft, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return HandoffDraft{}, fmt.Errorf("handoff goal cannot be empty")
	}

	currentSession, err := c.sessions.Get(ctx, sourceSessionID)
	if err != nil {
		return HandoffDraft{}, fmt.Errorf("failed to load source session: %w", err)
	}

	msgs, err := c.messages.List(ctx, sourceSessionID)
	if err != nil {
		return HandoffDraft{}, fmt.Errorf("failed to load source messages: %w", err)
	}

	// Apply SummaryMessageID truncation to avoid loading full uncompacted
	// history. Without this, sessions that went through morph-compact or
	// built-in summarization would feed the entire pre-summary transcript
	// to the background model, easily exceeding its context window.
	if currentSession.SummaryMessageID != "" {
		summaryMsgIndex := -1
		for i, msg := range msgs {
			if msg.ID == currentSession.SummaryMessageID {
				summaryMsgIndex = i
				break
			}
		}
		if summaryMsgIndex != -1 {
			slog.Debug("Handoff: truncating messages to summary point",
				"session_id", sourceSessionID,
				"summary_message_id", currentSession.SummaryMessageID,
				"original_count", len(msgs),
				"new_count", len(msgs)-summaryMsgIndex)
			msgs = msgs[summaryMsgIndex:]
		}
	}

	candidateFiles, err := c.collectHandoffCandidateFiles(ctx, sourceSessionID)
	if err != nil {
		return HandoffDraft{}, err
	}

	model, providerCfg, err := c.selectedModel(ctx, config.SelectedModelTypeBackground, false)
	if err != nil {
		return HandoffDraft{}, err
	}

	maxOutputTokens := int64(1500)
	if model.CatwalkCfg.DefaultMaxTokens > 0 && model.CatwalkCfg.DefaultMaxTokens < maxOutputTokens {
		maxOutputTokens = model.CatwalkCfg.DefaultMaxTokens
	}

	// Truncate messages to fit the background model's context window.
	// The handoff prompt is serialized as a single user message, so we
	// estimate character counts to avoid exceeding the model's input limit.
	msgs = truncateHandoffMessages(msgs, model.CatwalkCfg, maxOutputTokens)

	agent := fantasy.NewAgent(
		model.Model,
		fantasy.WithSystemPrompt(string(handoffPrompt)),
		fantasy.WithMaxOutputTokens(maxOutputTokens),
		fantasy.WithUserAgent(userAgent),
	)

	resp, err := agent.Stream(copilot.ContextWithInitiatorType(ctx, copilot.InitiatorAgent), fantasy.AgentStreamCall{
		Prompt:          buildHandoffPrompt(currentSession, goal, candidateFiles, msgs),
		ProviderOptions: getProviderOptions(model, providerCfg),
		PrepareStep: func(callCtx context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			callCtx = copilot.ContextWithInitiatorType(callCtx, copilot.InitiatorAgent)
			prepared.Messages = options.Messages
			if providerCfg.SystemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{
					fantasy.NewSystemMessage(providerCfg.SystemPromptPrefix),
				}, prepared.Messages...)
			}
			return callCtx, prepared, nil
		},
	})
	if err != nil {
		return HandoffDraft{}, fmt.Errorf("failed to generate handoff: %w", err)
	}
	if resp == nil {
		return HandoffDraft{}, fmt.Errorf("handoff generation returned no response")
	}

	draft, err := parseHandoffDraft(resp.Response.Content.Text(), candidateFiles)
	if err != nil {
		return HandoffDraft{}, err
	}
	if currentSession.PermissionMode == session.PermissionModeAuto {
		review, reviewErr := c.reviewHandoffText(ctx, currentSession, draft.Title, draft.Prompt, "")
		if reviewErr != nil {
			return HandoffDraft{}, fmt.Errorf("failed to review handoff draft: %w", reviewErr)
		}
		if !review.AllowAuto {
			return HandoffDraft{}, fmt.Errorf("handoff draft blocked by Auto Mode review: %s", strings.TrimSpace(review.Reason))
		}
	}
	return draft, nil
}

func (c *coordinator) selectedModel(ctx context.Context, modelType config.SelectedModelType, isSubAgent bool) (Model, config.ProviderConfig, error) {
	return c.selectedModelWithOverride(ctx, modelType, isSubAgent, nil)
}

func (c *coordinator) selectedModelWithOverride(
	ctx context.Context,
	modelType config.SelectedModelType,
	isSubAgent bool,
	override func(*config.SelectedModel),
) (Model, config.ProviderConfig, error) {
	selectedModel, ok := c.cfg.Config().SelectedModelForType(modelType)
	if !ok {
		return Model{}, config.ProviderConfig{}, fmt.Errorf("model type %q not configured", modelType)
	}
	if override != nil {
		override(&selectedModel)
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(selectedModel.Provider)
	if !ok {
		return Model{}, config.ProviderConfig{}, errModelProviderNotConfigured
	}

	var catwalkModel *catwalk.Model
	for i := range providerCfg.Models {
		if providerCfg.Models[i].ID == selectedModel.Model {
			catwalkModel = &providerCfg.Models[i]
			break
		}
	}
	if catwalkModel == nil {
		return Model{}, config.ProviderConfig{}, errTargetModelNotFound
	}

	thinkingDisabled := selectedModel.Think != nil && !*selectedModel.Think
	provider, err := c.buildProvider(providerCfg, *catwalkModel, isSubAgent, thinkingDisabled)
	if err != nil {
		return Model{}, config.ProviderConfig{}, err
	}

	modelID := selectedModel.Model
	if selectedModel.Provider == "openrouter" && isExactoSupported(modelID) {
		modelID += ":exacto"
	}

	languageModel, err := provider.LanguageModel(ctx, modelID)
	if err != nil {
		return Model{}, config.ProviderConfig{}, err
	}

	return Model{
		Model:      languageModel,
		CatwalkCfg: *catwalkModel,
		ModelCfg:   selectedModel,
	}, providerCfg, nil
}

func (c *coordinator) collectHandoffCandidateFiles(ctx context.Context, sessionID string) ([]string, error) {
	modifiedFiles, err := c.history.ListBySession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to list modified files: %w", err)
	}
	readFiles, err := c.filetracker.ListReadFiles(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to list read files: %w", err)
	}

	seen := make(map[string]struct{}, len(modifiedFiles)+len(readFiles))
	files := make([]string, 0, len(modifiedFiles)+len(readFiles))
	baseDir := ""
	if c.cfg != nil {
		baseDir = c.cfg.WorkingDir()
	}

	for _, file := range modifiedFiles {
		path := normalizeHandoffFilePath(baseDir, file.Path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		files = append(files, path)
	}
	for _, path := range readFiles {
		path = normalizeHandoffFilePath(baseDir, path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		files = append(files, path)
	}

	slices.Sort(files)
	return files, nil
}

func normalizeHandoffFilePath(baseDir, path string) string {
	path = filepath.Clean(path)
	if path == "." {
		return ""
	}
	if baseDir != "" {
		rel, err := filepath.Rel(filepath.Clean(baseDir), path)
		if err == nil {
			rel = filepath.Clean(rel)
			if rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return filepath.ToSlash(rel)
			}
		}
		if rel, ok := trimBaseDirPrefix(baseDir, path); ok {
			return rel
		}
	}
	return filepath.ToSlash(path)
}

func trimBaseDirPrefix(baseDir, path string) (string, bool) {
	candidate := filepath.ToSlash(filepath.Clean(path))
	if candidate == "" {
		return "", false
	}

	for _, base := range handoffBaseDirVariants(baseDir) {
		trimmedCandidate := strings.TrimPrefix(candidate, "/")
		prefix := base + "/"
		if strings.HasPrefix(trimmedCandidate, prefix) {
			return strings.TrimPrefix(trimmedCandidate, prefix), true
		}

		embeddedPrefix := "/" + prefix
		if idx := strings.Index(candidate, embeddedPrefix); idx >= 0 {
			return candidate[idx+len(embeddedPrefix):], true
		}
	}

	return "", false
}

func handoffBaseDirVariants(baseDir string) []string {
	base := filepath.ToSlash(filepath.Clean(baseDir))
	base = strings.Trim(base, "/")
	if base == "" {
		return nil
	}

	variants := []string{base}
	if volume := filepath.VolumeName(filepath.Clean(baseDir)); volume != "" {
		rootless := strings.Trim(filepath.ToSlash(strings.TrimPrefix(filepath.Clean(baseDir), volume)), "/")
		if rootless != "" && rootless != base {
			variants = append(variants, rootless)
		}
	}
	return variants
}

func buildHandoffPrompt(currentSession session.Session, goal string, candidateFiles []string, msgs []message.Message) string {
	var sb strings.Builder
	sb.WriteString("Goal for the new handoff session:\n")
	sb.WriteString(goal)
	sb.WriteString("\n\nCurrent session metadata:\n")
	sb.WriteString("- title: ")
	sb.WriteString(strings.TrimSpace(currentSession.Title))
	sb.WriteString("\n- collaboration_mode: ")
	sb.WriteString(string(currentSession.CollaborationMode))
	sb.WriteString("\n\nTracked files available to reference:\n")
	if len(candidateFiles) == 0 {
		sb.WriteString("- none\n")
	} else {
		for _, path := range candidateFiles {
			sb.WriteString("- ")
			sb.WriteString(path)
			sb.WriteByte('\n')
		}
	}

	sb.WriteString("\nTranscript:\n")
	for _, msg := range msgs {
		sb.WriteString("\n[")
		sb.WriteString(strings.ToUpper(string(msg.Role)))
		sb.WriteString("]\n")
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case message.TextContent:
				if strings.TrimSpace(p.Text) == "" {
					continue
				}
				sb.WriteString(p.Text)
				sb.WriteByte('\n')
			case message.ReasoningContent:
				continue
			case message.ToolCall:
				sb.WriteString("<tool_call name=\"")
				sb.WriteString(p.Name)
				sb.WriteString("\">\n")
				sb.WriteString(p.Input)
				sb.WriteString("\n</tool_call>\n")
			case message.ToolResult:
				sb.WriteString("<tool_result name=\"")
				sb.WriteString(p.Name)
				sb.WriteString("\" error=\"")
				if p.IsError {
					sb.WriteString("true")
				} else {
					sb.WriteString("false")
				}
				sb.WriteString("\">\n")
				sb.WriteString(p.ModelSafeContent())
				sb.WriteString("\n</tool_result>\n")
			case message.Finish:
				if p.Reason == "" {
					continue
				}
				sb.WriteString("<finish reason=\"")
				sb.WriteString(string(p.Reason))
				sb.WriteString("\" />\n")
			}
		}
	}

	return sb.String()
}

func parseHandoffDraft(raw string, candidateFiles []string) (HandoffDraft, error) {
	raw = strings.TrimSpace(raw)
	if matches := handoffCodeFenceRegex.FindStringSubmatch(raw); len(matches) == 2 {
		raw = strings.TrimSpace(matches[1])
	}
	if object := extractFirstJSONObject(raw); object != "" {
		raw = object
	} else if !strings.HasPrefix(raw, "{") {
		preview := raw
		if len(preview) > 200 {
			preview = preview[:200] + "…"
		}
		return HandoffDraft{}, fmt.Errorf("handoff generation returned unexpected response (possible API error): %.200s", preview)
	}

	var payload handoffResponse
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		// LLMs often emit unescaped backslashes (e.g., Windows paths like
		// D:\code\project) inside JSON string values. Fix them and retry.
		sanitized := sanitizeHandoffJSON(raw)
		if sanitizeErr := json.Unmarshal([]byte(sanitized), &payload); sanitizeErr != nil {
			return HandoffDraft{}, fmt.Errorf("failed to parse handoff response: %w", sanitizeErr)
		}
	}

	title := strings.TrimSpace(payload.Title)
	prompt := strings.TrimSpace(payload.Prompt)
	if title == "" || prompt == "" {
		return HandoffDraft{}, fmt.Errorf("handoff response must include non-empty title and prompt")
	}

	allowed := make(map[string]struct{}, len(candidateFiles))
	for _, path := range candidateFiles {
		allowed[normalizeDraftFilePath(path)] = struct{}{}
	}

	selected := make(map[string]struct{}, len(payload.RelevantFiles))
	for _, path := range payload.RelevantFiles {
		path = normalizeDraftFilePath(path)
		if path == "" {
			continue
		}
		if _, ok := allowed[path]; !ok {
			return HandoffDraft{}, fmt.Errorf("handoff response referenced unknown file %q", path)
		}
		selected[path] = struct{}{}
	}

	relevantFiles := make([]string, 0, len(selected))
	for _, path := range candidateFiles {
		normalized := normalizeDraftFilePath(path)
		if _, ok := selected[normalized]; ok {
			relevantFiles = append(relevantFiles, normalized)
		}
	}

	return HandoffDraft{
		Title:         title,
		Prompt:        prompt,
		RelevantFiles: relevantFiles,
	}, nil
}

// sanitizeHandoffJSON fixes unescaped backslashes inside JSON string values.
// LLMs frequently emit Windows-style paths (D:\code\project) or LaTeX-like
// sequences without properly escaping the backslashes. This function walks the
// raw JSON, and inside string literals it doubles any backslash that is not
// already part of a valid JSON escape sequence (\", \\, \/, \b, \f, \n, \r,
// \t, \uXXXX).
func sanitizeHandoffJSON(raw string) string {
	var sb strings.Builder
	sb.Grow(len(raw) + 64)

	inString := false
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if !inString {
			sb.WriteByte(ch)
			if ch == '"' {
				inString = true
			}
			continue
		}

		// Inside a JSON string.
		if ch == '"' {
			sb.WriteByte(ch)
			inString = false
			continue
		}

		if ch != '\\' {
			sb.WriteByte(ch)
			continue
		}

		// ch == '\\': check whether next char forms a valid JSON escape.
		if i+1 < len(raw) {
			next := raw[i+1]
			switch next {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				sb.WriteByte('\\')
				sb.WriteByte(next)
				i++
				continue
			case 'u':
				// \uXXXX — check if exactly 4 hex digits follow.
				if i+5 < len(raw) && isHex(raw[i+2]) && isHex(raw[i+3]) && isHex(raw[i+4]) && isHex(raw[i+5]) {
					sb.WriteString(raw[i : i+6])
					i += 5
					continue
				}
			}
		}

		// Not a valid escape: double the backslash so it becomes \\
		// which json.Unmarshal will decode to a single \.
		sb.WriteString("\\\\")
	}

	return sb.String()
}

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func normalizeDraftFilePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(path))
}

// truncateHandoffMessages removes older messages so the serialized handoff
// transcript fits within the background model's context window. We use a
// conservative char-per-token ratio of 3 to estimate token counts from the
// text-serialized transcript without expensive tokenization.
func truncateHandoffMessages(msgs []message.Message, modelCfg catwalk.Model, maxOutputTokens int64) []message.Message {
	contextWindow := int64(modelCfg.ContextWindow)
	if contextWindow <= 0 || len(msgs) <= 2 {
		return msgs
	}

	// Reserve tokens for: system prompt (~500), output, and safety margin.
	maxTranscriptTokens := contextWindow - maxOutputTokens - 2000
	if maxTranscriptTokens < 1000 {
		maxTranscriptTokens = contextWindow / 2
	}
	// Approximate 3 characters per token.
	maxTranscriptChars := maxTranscriptTokens * 3

	// Estimate total character count of the transcript.
	var totalChars int64
	for _, msg := range msgs {
		totalChars += estimateHandoffMessageChars(msg)
	}

	if totalChars <= maxTranscriptChars {
		return msgs
	}

	// Remove from the oldest messages first, keep at least 2.
	startIdx := 0
	for totalChars > maxTranscriptChars && startIdx < len(msgs)-2 {
		totalChars -= estimateHandoffMessageChars(msgs[startIdx])
		startIdx++
	}

	slog.Info("Truncated handoff messages to fit context window",
		"original_count", len(msgs),
		"new_count", len(msgs)-startIdx,
		"context_window", contextWindow,
		"max_transcript_tokens", maxTranscriptTokens)
	return msgs[startIdx:]
}

func estimateHandoffMessageChars(msg message.Message) int64 {
	var chars int64
	for _, part := range msg.Parts {
		switch p := part.(type) {
		case message.TextContent:
			chars += int64(len(p.Text))
		case message.ToolCall:
			chars += int64(len(p.Name) + len(p.Input) + 40)
		case message.ToolResult:
			chars += int64(len(p.Name) + len(p.ModelSafeContent()) + 40)
		}
	}
	return chars
}
