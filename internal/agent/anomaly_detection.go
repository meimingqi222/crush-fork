package agent

import (
	"log/slog"
	"strings"

	"github.com/charmbracelet/crush/internal/message"
)

// contentAnomalyKind identifies the type of content-level anomaly
// detected in an LLM response.
type contentAnomalyKind string

const (
	// anomalyRepetitiveText indicates the LLM generated the same line
	// 3+ times, constituting more than 50% of the response.
	anomalyRepetitiveText contentAnomalyKind = "repetitive_text"

	// anomalyDegenerateOutput indicates a near-empty response (≤5
	// runes) with no tool calls and no reasoning content.
	anomalyDegenerateOutput contentAnomalyKind = "degenerate_output"
)

const (
	// contentAnomalySnapshotInterval controls how often a periodic
	// context snapshot is logged (every N completed steps).
	contentAnomalySnapshotInterval = 5

	// contentAnomalyMinRepeatLines is the minimum number of identical
	// lines required to flag repetitive text.
	contentAnomalyMinRepeatLines = 3

	// contentAnomalyMinRepeatRunes is the minimum rune length for a
	// line to be considered in repetition analysis. Short lines like
	// "---" or "}" are ignored.
	contentAnomalyMinRepeatRunes = 4

	// contentAnomalyRepeatRatio is the minimum fraction of qualifying
	// lines that must be the repeated line to trigger detection.
	contentAnomalyRepeatRatio = 0.5

	// contentAnomalyDegenerateMaxRunes is the maximum text length (in
	// runes) for a "degenerate output" detection.
	contentAnomalyDegenerateMaxRunes = 5
)

// contentAnomaly holds the result of a single anomaly detection.
type contentAnomaly struct {
	Kind         contentAnomalyKind
	RepeatedLine string
	RepeatCount  int
	RepeatRatio  float64
}

// hasRepetitiveLines detects whether text contains a single line
// repeated 3+ times that constitutes more than 50% of all qualifying
// lines. Returns whether repetition was found, the repeated line, its
// count, and the total number of qualifying lines.
func hasRepetitiveLines(text string) (bool, string, int, int) {
	lines := strings.Split(text, "\n")
	freq := make(map[string]int)
	totalQualifying := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || len([]rune(trimmed)) < contentAnomalyMinRepeatRunes {
			continue
		}
		freq[trimmed]++
		totalQualifying++
	}

	if totalQualifying == 0 {
		return false, "", 0, 0
	}

	var maxLine string
	var maxCount int
	for line, count := range freq {
		if count > maxCount {
			maxCount = count
			maxLine = line
		}
	}

	if maxCount < contentAnomalyMinRepeatLines {
		return false, "", 0, 0
	}

	ratio := float64(maxCount) / float64(totalQualifying)
	if ratio < contentAnomalyRepeatRatio {
		return false, "", 0, 0
	}

	return true, maxLine, maxCount, totalQualifying
}

// isDegenerateOutput detects a near-empty response (≤5 runes) with no
// tool calls and no reasoning content. This catches responses that slip
// past the empty stream detector (which requires zero text).
func isDegenerateOutput(assistant *message.Message) bool {
	if assistant == nil {
		return false
	}
	text := strings.TrimSpace(assistant.Content().Text)
	if text == "" {
		return false
	}
	if len([]rune(text)) > contentAnomalyDegenerateMaxRunes {
		return false
	}
	if len(assistant.ToolCalls()) > 0 {
		return false
	}
	if assistant.ReasoningContent().Thinking != "" {
		return false
	}
	reason := assistant.FinishReason()
	return reason == message.FinishReasonEndTurn || reason == message.FinishReasonUnknown
}

// detectContentAnomalies runs all content-level anomaly detectors on
// the assistant message and returns any anomalies found. Multiple
// anomaly types can be detected for the same response.
func detectContentAnomalies(assistant *message.Message) []contentAnomaly {
	if assistant == nil {
		return nil
	}
	var anomalies []contentAnomaly

	text := assistant.Content().Text
	if repeated, line, count, total := hasRepetitiveLines(text); repeated {
		anomalies = append(anomalies, contentAnomaly{
			Kind:         anomalyRepetitiveText,
			RepeatedLine: line,
			RepeatCount:  count,
			RepeatRatio:  float64(count) / float64(total),
		})
	}

	if isDegenerateOutput(assistant) {
		anomalies = append(anomalies, contentAnomaly{
			Kind: anomalyDegenerateOutput,
		})
	}

	return anomalies
}

// logContextSnapshot logs a periodic state snapshot at INFO level for
// diagnostic correlation. Called every contentAnomalySnapshotInterval
// completed steps.
func logContextSnapshot(
	sessionID, model, provider string,
	completedSteps, runToolUses int,
	runLastTool string,
	assistant *message.Message,
	estimatedPromptTokens int64,
	messageCount int,
) {
	text := ""
	textLen := 0
	hasToolCalls := false
	toolCallCount := 0
	hasReasoning := false
	finishReason := message.FinishReason("")

	if assistant != nil {
		text = strings.TrimSpace(assistant.Content().Text)
		textLen = len([]rune(text))
		hasToolCalls = len(assistant.ToolCalls()) > 0
		toolCallCount = len(assistant.ToolCalls())
		hasReasoning = assistant.ReasoningContent().Thinking != ""
		finishReason = assistant.FinishReason()
	}

	slog.Info("Anomaly context snapshot",
		"session_id", sessionID,
		"model", model,
		"provider", provider,
		"completed_steps", completedSteps,
		"run_tool_uses", runToolUses,
		"run_last_tool", runLastTool,
		"finish_reason", finishReason,
		"text_length", textLen,
		"text_preview", previewText(text, 300),
		"tool_call_count", toolCallCount,
		"has_tool_calls", hasToolCalls,
		"has_reasoning", hasReasoning,
		"estimated_prompt_tokens", estimatedPromptTokens,
		"message_count", messageCount,
	)
}

// logAnomalyDiagnostic logs a detailed anomaly report at WARN level
// when a content-level anomaly is detected.
func logAnomalyDiagnostic(
	anomaly contentAnomaly,
	sessionID, model, provider string,
	completedSteps, runToolUses int,
	runLastTool string,
	assistant *message.Message,
	estimatedPromptTokens int64,
	messageCount int,
) {
	text := ""
	textLen := 0
	hasToolCalls := false
	toolCallCount := 0
	hasReasoning := false
	finishReason := message.FinishReason("")

	if assistant != nil {
		text = strings.TrimSpace(assistant.Content().Text)
		textLen = len([]rune(text))
		hasToolCalls = len(assistant.ToolCalls()) > 0
		toolCallCount = len(assistant.ToolCalls())
		hasReasoning = assistant.ReasoningContent().Thinking != ""
		finishReason = assistant.FinishReason()
	}

	args := []any{
		"anomaly_kind", anomaly.Kind,
		"session_id", sessionID,
		"model", model,
		"provider", provider,
		"completed_steps", completedSteps,
		"run_tool_uses", runToolUses,
		"run_last_tool", runLastTool,
		"finish_reason", finishReason,
		"text_length", textLen,
		"text_preview", previewText(text, 500),
		"tool_call_count", toolCallCount,
		"has_tool_calls", hasToolCalls,
		"has_reasoning", hasReasoning,
		"estimated_prompt_tokens", estimatedPromptTokens,
		"message_count", messageCount,
	}

	if anomaly.Kind == anomalyRepetitiveText {
		args = append(args,
			"repeated_line", previewText(anomaly.RepeatedLine, 200),
			"repeat_count", anomaly.RepeatCount,
			"repeat_ratio", anomaly.RepeatRatio,
		)
	}

	slog.Warn("Anomaly detected in assistant response", args...)
}
