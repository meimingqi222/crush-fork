package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsContextWindowExceededError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "non-provider error",
			err:  assert.AnError,
			want: false,
		},
		{
			name: "provider error status 429",
			err:  &fantasy.ProviderError{StatusCode: 429, Message: "rate limit exceeded"},
			want: false,
		},
		{
			name: "provider error 400 unrelated message",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: "invalid model"},
			want: false,
		},
		{
			name: "context window phrase",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: "Your input exceeds the context window of this model"},
			want: true,
		},
		{
			name: "context length phrase",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: "This model's maximum context length is 128000 tokens"},
			want: true,
		},
		{
			name: "maximum context phrase",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: "maximum context exceeded"},
			want: true,
		},
		{
			name: "input exceeds phrase",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: "input exceeds the limit"},
			want: true,
		},
		{
			name: "too many tokens phrase",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: "too many tokens in the request"},
			want: true,
		},
		{
			name: "prompt is too long phrase",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: "prompt is too long: 450000 tokens"},
			want: true,
		},
		{
			name: "case insensitive match",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: "CONTEXT WINDOW EXCEEDED"},
			want: true,
		},
		{
			name: "double-encoded JSON from copilot-api (context window)",
			err: &fantasy.ProviderError{
				StatusCode: 400,
				Message:    `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","code":"invalid_request_body"}}`,
			},
			want: true,
		},
		{
			name: "input length range phrase",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: "Error code: 400 - {'error': {'message': '<400> InternalError.Algo.InvalidParameter: Range of input length should be [1, 202752]', 'type': 'invalid_request_error', 'param': None, 'code': 'invalid_parameter_error'}}"},
			want: true,
		},
		{
			name: "request body too large",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: `{"type":"error", "error":{"type":"api_error", "message": "Request body too large."}}`},
			want: true,
		},
		{
			name: "request entity too large status",
			err:  &fantasy.ProviderError{StatusCode: 413, Message: "Request Entity Too Large"},
			want: true,
		},
		{
			name: "exceeded model token limit phrase",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: "exceeded model token limit"},
			want: true,
		},
		{
			name: "greater than context length phrase",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: "prompt is greater than the context length"},
			want: true,
		},
		{
			name: "openai model context window exceeded code",
			err:  &fantasy.ProviderError{StatusCode: 400, Message: `{"error":{"code":"model_context_window_exceeded","message":"The request exceeds the model's context window."}}`},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isContextWindowExceededError(tt.err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTruncateOversizedToolResults(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	agent := &sessionAgent{messages: env.messages, workingDir: env.workingDir}

	sess, err := env.sessions.Create(t.Context(), "Truncate Test")
	require.NoError(t, err)

	// Create a tool message whose Content is larger than the threshold.
	bigContent := strings.Repeat("x", contextWindowToolResultMaxChars+1000)
	toolMsg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "tc-1",
				Name:       "query_db",
				Content:    bigContent,
			},
		},
	})
	require.NoError(t, err)
	_ = toolMsg

	err = agent.truncateOversizedToolResults(t.Context(), sess.ID)
	require.NoError(t, err)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	results := msgs[0].ToolResults()
	require.Len(t, results, 1)

	truncated := results[0].Content
	// Content must be shorter than original.
	assert.Less(t, len(truncated), len(bigContent))
	assert.LessOrEqual(t, len([]rune(truncated)), contextWindowToolResultMaxChars)
	assert.True(t, strings.HasPrefix(truncated, strings.Repeat("x", 1000)))
	// Must include truncation notice.
	assert.Contains(t, truncated, "characters omitted")
	assert.Contains(t, truncated, "This excerpt is incomplete")
	assert.Contains(t, truncated, ".crush/truncation/")
	assert.Contains(t, truncated, "Use the read tool")
	assert.Contains(t, truncated, "offset/limit")
	assert.Contains(t, truncated, "grep")
	assert.Contains(t, truncated, "do not guess")

	artifactMatches, globErr := filepath.Glob(filepath.Join(env.workingDir, ".crush", "truncation", "*.txt"))
	require.NoError(t, globErr)
	require.Len(t, artifactMatches, 1)
	artifactContent, readErr := os.ReadFile(artifactMatches[0])
	require.NoError(t, readErr)
	assert.Equal(t, bigContent, string(artifactContent))
}

func TestTruncateOversizedToolResults_UnicodeSafe(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	agent := &sessionAgent{messages: env.messages, workingDir: env.workingDir}

	sess, err := env.sessions.Create(t.Context(), "Truncate Unicode")
	require.NoError(t, err)

	bigUnicodeContent := strings.Repeat("你", contextWindowToolResultMaxChars+10)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "tc-uni",
				Name:       "query_db",
				Content:    bigUnicodeContent,
			},
		},
	})
	require.NoError(t, err)

	err = agent.truncateOversizedToolResults(t.Context(), sess.ID)
	require.NoError(t, err)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	results := msgs[0].ToolResults()
	require.Len(t, results, 1)
	truncated := results[0].Content

	assert.True(t, utf8.ValidString(truncated), "truncated content must remain valid UTF-8")
	assert.Contains(t, truncated, ".crush/truncation/")
	assert.Contains(t, truncated, "This excerpt is incomplete")
	assert.Contains(t, truncated, "do not guess")
	assert.LessOrEqual(t, len([]rune(truncated)), contextWindowToolResultMaxChars)
	assert.True(t, strings.HasPrefix(truncated, strings.Repeat("你", 1000)))
}

func TestEnforceStepToolResultBudget_TruncatesOversizedStepAggregate(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	agent := &sessionAgent{messages: env.messages, workingDir: env.workingDir}
	used := 35_000
	original := strings.Repeat("x", 10_000)
	result := message.ToolResult{Name: "glob", Content: original}

	truncated := agent.enforceStepToolResultBudget("sess", result, &used)

	assert.True(t, utf8.ValidString(truncated.Content))
	assert.Contains(t, truncated.Content, "This excerpt is incomplete")
	assert.Contains(t, truncated.Content, ".crush/truncation/")
	assert.LessOrEqual(t, len([]rune(truncated.Content)), 5_000)
	assert.Equal(t, contextWindowStepToolResultCharsLimit, used)

	artifactMatches, globErr := filepath.Glob(filepath.Join(env.workingDir, ".crush", "truncation", "*.txt"))
	require.NoError(t, globErr)
	require.Len(t, artifactMatches, 1)
	artifactContent, readErr := os.ReadFile(artifactMatches[0])
	require.NoError(t, readErr)
	assert.Equal(t, original, string(artifactContent))
}

func TestEnforceStepToolResultBudget_PreviewsWhenBudgetExhausted(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	used := contextWindowStepToolResultCharsLimit
	original := strings.Repeat("head", 200) + strings.Repeat("tail", 600)
	result := message.ToolResult{Name: "glob", Content: original}

	truncated := agent.enforceStepToolResultBudget("sess", result, &used)

	assert.Contains(t, truncated.Content, "Step tool-result budget exhausted")
	assert.Contains(t, truncated.Content, "Showing the last 2000 of 3200 characters")
	assert.Contains(t, truncated.Content, "1200 characters omitted")
	assert.Contains(t, truncated.Content, "do not guess")
	assert.Contains(t, truncated.Content, "offset/limit")
	assert.True(t, strings.HasPrefix(truncated.Content, strings.Repeat("tail", 500)))
	assert.NotContains(t, truncated.Content, strings.Repeat("head", 20))
	assert.Equal(t, contextWindowStepToolResultCharsLimit, used)
}

func TestTruncateOversizedToolResults_SmallContentUntouched(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	agent := &sessionAgent{messages: env.messages, workingDir: env.workingDir}

	sess, err := env.sessions.Create(t.Context(), "Truncate Small")
	require.NoError(t, err)

	smallContent := "SELECT * FROM users LIMIT 10"
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "tc-2",
				Name:       "query_db",
				Content:    smallContent,
			},
		},
	})
	require.NoError(t, err)

	err = agent.truncateOversizedToolResults(t.Context(), sess.ID)
	require.NoError(t, err)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	results := msgs[0].ToolResults()
	require.Len(t, results, 1)
	assert.Equal(t, smallContent, results[0].Content, "small content should be unchanged")
}

func TestTruncateOversizedToolResults_ErrorResultUntouched(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	agent := &sessionAgent{messages: env.messages, workingDir: env.workingDir}

	sess, err := env.sessions.Create(t.Context(), "Truncate Error")
	require.NoError(t, err)

	// Error results should never be truncated even if large.
	bigError := strings.Repeat("e", contextWindowToolResultMaxChars+500)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "tc-3",
				Name:       "query_db",
				Content:    bigError,
				IsError:    true,
			},
		},
	})
	require.NoError(t, err)

	err = agent.truncateOversizedToolResults(t.Context(), sess.ID)
	require.NoError(t, err)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	results := msgs[0].ToolResults()
	require.Len(t, results, 1)
	assert.Equal(t, bigError, results[0].Content, "error results should not be truncated")

	artifactMatches, globErr := filepath.Glob(filepath.Join(env.workingDir, ".crush", "truncation", "*.txt"))
	require.NoError(t, globErr)
	assert.Empty(t, artifactMatches)
}

func TestEnforceStepToolResultBudget_PreviewsWhenBudgetExhausted_PersistsFullOutput(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	agent := &sessionAgent{workingDir: env.workingDir}
	used := contextWindowStepToolResultCharsLimit
	original := strings.Repeat("head", 200) + strings.Repeat("tail", 600)
	result := message.ToolResult{Name: "glob", Content: original}

	truncated := agent.enforceStepToolResultBudget("sess", result, &used)

	assert.Contains(t, truncated.Content, "Step tool-result budget exhausted")
	assert.Contains(t, truncated.Content, ".crush/truncation/")
	assert.Contains(t, truncated.Content, "The full output was saved")
	assert.Contains(t, truncated.Content, "Use the read tool with offset/limit or grep")
	assert.True(t, strings.HasPrefix(truncated.Content, strings.Repeat("tail", 500)))
	assert.NotContains(t, truncated.Content, strings.Repeat("head", 20))
	assert.Equal(t, contextWindowStepToolResultCharsLimit, used)

	artifactMatches, globErr := filepath.Glob(filepath.Join(env.workingDir, ".crush", "truncation", "*.txt"))
	require.NoError(t, globErr)
	require.Len(t, artifactMatches, 1)
	artifactContent, readErr := os.ReadFile(artifactMatches[0])
	require.NoError(t, readErr)
	assert.Equal(t, original, string(artifactContent))
}

func TestEnforceMessageToolResultBudget_PreviewsWhenBudgetExhausted_PersistsFullOutput(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	agent := &sessionAgent{workingDir: env.workingDir}
	messageOriginal := strings.Repeat("head", 200) + strings.Repeat("tail", 600)
	results := []message.ToolResult{
		{Name: "glob", Content: strings.Repeat("a", contextWindowMessageToolResultCharsLimit)},
		{Name: "grep", Content: messageOriginal},
	}

	budgeted := agent.enforceMessageToolResultBudget("sess", results)

	require.Len(t, budgeted, 2)
	assert.Equal(t, results[0].Content, budgeted[0].Content)
	assert.Contains(t, budgeted[1].Content, "Message tool-result budget exhausted")
	assert.Contains(t, budgeted[1].Content, ".crush/truncation/")
	assert.Contains(t, budgeted[1].Content, "The full output was saved")
	assert.Contains(t, budgeted[1].Content, "Use the read tool with offset/limit or grep")
	assert.True(t, strings.HasPrefix(budgeted[1].Content, strings.Repeat("tail", 500)))
	assert.NotContains(t, budgeted[1].Content, strings.Repeat("head", 20))

	artifactMatches, globErr := filepath.Glob(filepath.Join(env.workingDir, ".crush", "truncation", "*.txt"))
	require.NoError(t, globErr)
	require.Len(t, artifactMatches, 1)
	artifactContent, readErr := os.ReadFile(artifactMatches[0])
	require.NoError(t, readErr)
	assert.Equal(t, messageOriginal, string(artifactContent))
}
