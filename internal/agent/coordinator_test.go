package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/timeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSessionAgent is a minimal mock for the SessionAgent interface.
type mockSessionAgent struct {
	model        Model
	runFunc      func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error)
	estimateFunc func(ctx context.Context, sessionID string, model Model) (int64, error)
	summarizeErr error
	summarized   []string
	cancelled    []string
}

func (m *mockSessionAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	return m.runFunc(ctx, call)
}

func (m *mockSessionAgent) EstimateSessionPromptTokensForModel(ctx context.Context, sessionID string, model Model) (int64, error) {
	if m.estimateFunc == nil {
		return 0, nil
	}
	return m.estimateFunc(ctx, sessionID, model)
}

func (m *mockSessionAgent) Model() Model                                    { return m.model }
func (m *mockSessionAgent) SetModels(large, small Model)                    {}
func (m *mockSessionAgent) SetTools(tools []fantasy.AgentTool)              {}
func (m *mockSessionAgent) SetSystemPrompt(systemPrompt string)             {}
func (m *mockSessionAgent) SetSystemPromptPrefix(systemPromptPrefix string) {}
func (m *mockSessionAgent) Cancel(sessionID string) {
	m.cancelled = append(m.cancelled, sessionID)
}
func (m *mockSessionAgent) CancelAll()                                          {}
func (m *mockSessionAgent) IsSessionBusy(sessionID string) bool                 { return false }
func (m *mockSessionAgent) IsBusy() bool                                        { return false }
func (m *mockSessionAgent) QueuedPrompts(sessionID string) int                  { return 0 }
func (m *mockSessionAgent) QueuedPromptsList(sessionID string) []string         { return nil }
func (m *mockSessionAgent) RemoveQueuedPrompt(sessionID string, index int) bool { return false }
func (m *mockSessionAgent) ClearQueue(sessionID string)                         {}
func (m *mockSessionAgent) PauseQueue(sessionID string)                         {}
func (m *mockSessionAgent) ResumeQueue(sessionID string)                        {}
func (m *mockSessionAgent) IsQueuePaused(sessionID string) bool                 { return false }
func (m *mockSessionAgent) PrioritizeQueuedPrompt(sessionID string, index int) bool {
	return false
}

func (m *mockSessionAgent) Summarize(context.Context, string, fantasy.ProviderOptions) error {
	if m.summarizeErr != nil {
		return m.summarizeErr
	}
	m.summarized = append(m.summarized, "summarized")
	return nil
}

func (m *mockSessionAgent) RespondAsBackground(_ context.Context, _, _ string) (string, error) {
	return "mock irc reply", nil
}

// newTestCoordinator creates a minimal coordinator for unit testing runSubAgent.
func newTestCoordinator(t *testing.T, env fakeEnv, providerID string, providerCfg config.ProviderConfig) *coordinator {
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, providerCfg)
	env.sessions.SetDefaultPermissionMode(session.PermissionModeDefault)
	return &coordinator{
		cfg:              cfg,
		sessions:         env.sessions,
		messages:         env.messages,
		permissions:      env.permissions,
		escalationBridge: permission.NewEscalationBridge(),
	}
}

// newMockAgent creates a mockSessionAgent with the given provider and run function.
func newMockAgent(providerID string, maxTokens int64, runFunc func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)) *mockSessionAgent {
	return &mockSessionAgent{
		model: Model{
			CatwalkCfg: catwalk.Model{
				DefaultMaxTokens: maxTokens,
			},
			ModelCfg: config.SelectedModel{
				Provider: providerID,
			},
		},
		runFunc: runFunc,
	}
}

// agentResultWithText creates a minimal AgentResult with the given text response.
func agentResultWithText(text string) *fantasy.AgentResult {
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: text},
			},
		},
	}
}

func TestRunSubAgent(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	t.Run("happy path", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			assert.Equal(t, "do something", call.Prompt)
			assert.Equal(t, int64(4096), call.MaxOutputTokens)
			_, createErr := env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					message.ToolResult{Name: agenttools.YieldToolName}.WithYield(message.ToolResultYield{
						Status: string(message.ToolResultSubtaskStatusCompleted),
						Data:   "done",
					}),
				},
			})
			require.NoError(t, createErr)
			return agentResultWithText("done"), nil
		})

		resp, err := coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "do something",
			SessionTitle:    "Test Session",
		})
		require.NoError(t, err)
		assert.Equal(t, "done", resp.Content)
		assert.False(t, resp.IsError)
		parsed, ok := message.ParseToolResultSubtaskResult(resp.Metadata)
		require.True(t, ok)
		assert.Equal(t, message.ToolResultSubtaskResult{
			ChildSessionID:   "msg-1$$call-1",
			ParentToolCallID: "call-1",
			ParentMessageID:  "msg-1",
			Status:           message.ToolResultSubtaskStatusCompleted,
		}, parsed)
		yield, ok := message.ParseToolResultYield(resp.Metadata)
		require.True(t, ok)
		assert.Equal(t, string(message.ToolResultSubtaskStatusCompleted), yield.Status)
	})

	t.Run("publishes spawn-time started timeline event", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)
		// Inject a timeline service so the coordinator can publish the
		// spawn-time SubagentEventStarted -> ChildSessionStartedEvent.
		tl := timeline.NewService()
		coord.timeline = tl

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			_, createErr := env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					message.ToolResult{Name: agenttools.YieldToolName}.WithYield(message.ToolResultYield{
						Status: string(message.ToolResultSubtaskStatusCompleted),
						Data:   "done",
					}),
				},
			})
			require.NoError(t, createErr)
			return agentResultWithText("done"), nil
		})

		_, err = coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "do something",
			SessionTitle:    "Test Session",
		})
		require.NoError(t, err)

		// The spawn-time started event must have been published to the
		// parent's timeline, recording the child session ID association.
		events := tl.ListBySession(parentSession.ID)
		var started *timeline.Event
		for i := range events {
			if events[i].Type == timeline.EventChildSessionStarted {
				started = &events[i]
				break
			}
		}
		require.NotNil(t, started, "expected a ChildSessionStarted timeline event at spawn time")
		assert.Equal(t, "msg-1$$call-1", started.ChildSessionID)
		assert.Equal(t, "Test Session", started.Title)
	})

	t.Run("auto mode blocks delegation when handoff review cannot run", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)
		coord.cfg.Config().Models[config.SelectedModelTypeAutoClassifier] = config.SelectedModel{
			Provider: "missing-provider",
			Model:    "missing-model",
		}

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		_, err = env.sessions.UpdatePermissionMode(t.Context(), parentSession.ID, session.PermissionModeAuto)
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			t.Fatalf("subagent should not run when auto delegation review blocks")
			return nil, nil
		})

		resp, err := coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "do something",
			SessionTitle:    "Test Session",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Auto Mode blocked subagent delegation because the handoff review failed.", resp.Content)
	})

	t.Run("ModelCfg.MaxTokens overrides default", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := &mockSessionAgent{
			model: Model{
				CatwalkCfg: catwalk.Model{
					DefaultMaxTokens: 4096,
				},
				ModelCfg: config.SelectedModel{
					Provider:  providerID,
					MaxTokens: 8192,
				},
			},
			runFunc: func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
				assert.Equal(t, int64(8192), call.MaxOutputTokens)
				return agentResultWithText("ok"), nil
			},
		}

		resp, err := coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
		})
		require.NoError(t, err)
		assert.Equal(t, "ok", resp.Content)
	})

	t.Run("session creation failure with canceled context", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, nil)

		// Use a canceled context to trigger CreateTaskSession failure.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err = coord.runSubAgentDirect(ctx, subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
		})
		require.Error(t, err)
	})

	t.Run("provider not configured", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		// Agent references a provider that doesn't exist in config.
		agent := newMockAgent("unknown-provider", 4096, nil)

		_, err = coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "model provider not configured")
	})

	t.Run("agent run error returns error response", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, errors.New("agent exploded")
		})

		resp, err := coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
		})
		// runSubAgent returns (errorResponse, nil) when agent.Run fails 鈥?not a Go error.
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "agent exploded", resp.Content)
		parsed, ok := message.ParseToolResultSubtaskResult(resp.Metadata)
		require.True(t, ok)
		assert.Equal(t, message.ToolResultSubtaskResult{
			ChildSessionID:   "msg-1$$call-1",
			ParentToolCallID: "call-1",
			ParentMessageID:  "msg-1",
			Status:           message.ToolResultSubtaskStatusFailed,
		}, parsed)
	})

	t.Run("agent run error prefers persisted child assistant error details", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			assistant, createErr := env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
				Role: message.Assistant,
			})
			require.NoError(t, createErr)

			assistant.AddFinish(message.FinishReasonError, "Network error", "stream idle timeout: no data received for 45s")
			require.NoError(t, env.messages.Update(ctx, assistant))

			return nil, errors.New("agent exploded")
		})

		resp, err := coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "stream idle timeout: no data received for 45s", resp.Content)
	})

	t.Run("falls back to persisted child session assistant content", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			_, err := env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "persisted final answer"},
					message.Finish{Reason: message.FinishReasonEndTurn},
				},
			})
			require.NoError(t, err)
			return &fantasy.AgentResult{}, nil
		})

		resp, err := coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "persisted final answer", resp.Content)
	})

	t.Run("uses persisted yield metadata when available", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			_, err := env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					message.ToolResult{Name: agenttools.YieldToolName}.WithYield(message.ToolResultYield{
						Status: string(message.ToolResultSubtaskStatusCompletedWithWarnings),
						Data:   "structured summary",
						Error:  "minor warning",
					}),
				},
			})
			require.NoError(t, err)
			return &fantasy.AgentResult{}, nil
		})

		resp, err := coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
			SubagentType:    config.AgentGeneral,
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "structured summary", resp.Content)
		subtask, ok := message.ParseToolResultSubtaskResult(resp.Metadata)
		require.True(t, ok)
		assert.Equal(t, message.ToolResultSubtaskStatusCompletedWithWarnings, subtask.Status)
		yield, ok := message.ParseToolResultYield(resp.Metadata)
		require.True(t, ok)
		assert.Equal(t, string(message.ToolResultSubtaskStatusCompletedWithWarnings), yield.Status)
	})

	t.Run("uses yield data over assistant content when both are present", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			_, err := env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "full final answer with details"},
					message.Finish{Reason: message.FinishReasonEndTurn},
				},
			})
			require.NoError(t, err)
			_, err = env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					message.ToolResult{Name: agenttools.YieldToolName}.WithYield(message.ToolResultYield{
						Status: string(message.ToolResultSubtaskStatusCompleted),
						Data:   "short structured summary",
					}),
				},
			})
			require.NoError(t, err)
			return &fantasy.AgentResult{}, nil
		})

		resp, err := coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
			SubagentType:    config.AgentGeneral,
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "short structured summary", resp.Content)
		yield, ok := message.ParseToolResultYield(resp.Metadata)
		require.True(t, ok)
		assert.Equal(t, "short structured summary", yield.Data)
	})

	t.Run("uses yield data when subagent calls yield", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			_, err := env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "The task is complete."},
					message.Finish{Reason: message.FinishReasonEndTurn},
				},
			})
			require.NoError(t, err)
			_, err = env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					message.ToolResult{Name: agenttools.YieldToolName}.WithYield(message.ToolResultYield{
						Data:   "detailed subagent report via yield",
						Status: "completed",
					}),
				},
			})
			require.NoError(t, err)
			return agentResultWithText("The task is complete."), nil
		})

		resp, err := coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
			SubagentType:    config.AgentGeneral,
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "detailed subagent report via yield", resp.Content)
	})

	t.Run("missing finish policy warns when finish is absent", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)
		coord.cfg.Config().Subagents = &config.SubagentRuntimeConfig{StructuredCompletionRequired: true, MissingFinishPolicy: "warn"}

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			if strings.Contains(call.Prompt, "Call yield exactly once now") {
				return agentResultWithText("reminder ignored"), nil
			}
			_, err := env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
				Role:  message.Assistant,
				Parts: []message.ContentPart{message.TextContent{Text: "fallback text"}, message.Finish{Reason: message.FinishReasonEndTurn}},
			})
			require.NoError(t, err)
			return &fantasy.AgentResult{}, nil
		})

		resp, err := coord.runSubAgentDirect(t.Context(), subAgentParams{Agent: agent, SessionID: parentSession.ID, AgentMessageID: "msg-1", ParentMessageID: "msg-1", ToolCallID: "call-1", Prompt: "test", SessionTitle: "Test", SubagentType: config.AgentGeneral})
		require.NoError(t, err)
		yield, ok := message.ParseToolResultYield(resp.Metadata)
		require.True(t, ok)
		assert.Equal(t, string(message.ToolResultSubtaskStatusCompletedWithWarnings), yield.Status)
		assert.Equal(t, "fallback text", yield.Data)
	})

	t.Run("returns guidance text when neither result nor child session has content", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return &fantasy.AgentResult{}, nil
		})

		resp, err := coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Contains(t, resp.Content, "Subagent completed with no textual response")
	})

	t.Run("does not fall back to earlier assistant text when latest assistant is empty", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			_, err := env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "earlier text"},
					message.Finish{Reason: message.FinishReasonToolUse},
				},
			})
			require.NoError(t, err)

			_, err = env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.Finish{Reason: message.FinishReasonEndTurn},
				},
			})
			require.NoError(t, err)

			return &fantasy.AgentResult{}, nil
		})

		resp, err := coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Contains(t, resp.Content, "Subagent completed with no textual response")
	})

	t.Run("session setup callback is invoked", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		var setupCalledWith string
		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
			SessionSetup: func(sessionID string) {
				setupCalledWith = sessionID
			},
		})
		require.NoError(t, err)
		assert.NotEmpty(t, setupCalledWith, "SessionSetup should have been called")
	})

	t.Run("propagates subagent lifecycle policy in context", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		background := true
		agent := newMockAgent(providerID, 4096, func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			require.Equal(t, "isolated", agenttools.GetAgentMemoryFromContext(ctx))
			require.Equal(t, "session", agenttools.GetAgentIsolationFromContext(ctx))
			require.True(t, agenttools.GetAgentBackgroundFromContext(ctx))
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
			AgentMemory:     "isolated",
			AgentIsolation:  "session",
			AgentBackground: &background,
		})
		require.NoError(t, err)
	})

	t.Run("propagates approval metadata in worker identity", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			identity := permission.WorkerIdentityFromContext(ctx)
			require.Equal(t, parentSession.ID, identity.ParentSessionID)
			require.Equal(t, "call-1", identity.TaskID)
			require.Equal(t, config.AgentGeneral, identity.ProfileName)
			require.NotEmpty(t, identity.ChildSessionID)
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
			SubagentType:    config.AgentGeneral,
		})
		require.NoError(t, err)
	})

	t.Run("falls back from worktree isolation and propagates workspace cwd", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		// Use a temp dir outside the git repo so worktree creation fails and
		// the coordinator falls back to session isolation.
		nonGitDir := t.TempDir()

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		parentSession.WorkspaceCWD = nonGitDir
		parentSession, err = env.sessions.Save(t.Context(), parentSession)
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			require.Equal(t, "session", agenttools.GetAgentIsolationFromContext(ctx))
			require.Equal(t, nonGitDir, agenttools.GetWorkingDirFromContext(ctx))
			return agentResultWithText("ok"), nil
		})

		resp, err := coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-worktree",
			Prompt:          "test",
			SessionTitle:    "Test",
			AgentIsolation:  "worktree",
		})
		require.NoError(t, err)

		subtask, ok := message.ParseToolResultSubtaskResult(resp.Metadata)
		require.True(t, ok)
		require.NotEmpty(t, subtask.ChildSessionID)

		childSession, err := env.sessions.Get(t.Context(), subtask.ChildSessionID)
		require.NoError(t, err)
		require.Equal(t, nonGitDir, childSession.WorkspaceCWD)
	})

	t.Run("clears inherited parent runtime config before subagent run", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		parentRuntimeConfig := sessionAgentRuntimeConfig{
			MaxOutputTokens: 1234,
		}

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			got, ok := ctx.Value(sessionAgentRuntimeConfigContextKey{}).(*sessionAgentRuntimeConfig)
			require.True(t, ok, "subagent context should carry an explicit runtime config override marker")
			require.Nil(t, got, "subagent must not inherit the parent agent runtime config")
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgentDirect(
			context.WithValue(t.Context(), sessionAgentRuntimeConfigContextKey{}, parentRuntimeConfig),
			subAgentParams{
				Agent:          agent,
				SessionID:      parentSession.ID,
				AgentMessageID: "msg-1",
				ToolCallID:     "call-1",
				Prompt:         "test",
				SessionTitle:   "Test",
			},
		)
		require.NoError(t, err)
	})

	t.Run("cost propagation to parent session", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			// Simulate the agent incurring cost by updating the child session.
			childSession, err := env.sessions.Get(ctx, call.SessionID)
			if err != nil {
				return nil, err
			}
			childSession.Cost = 0.05
			_, err = env.sessions.Save(ctx, childSession)
			if err != nil {
				return nil, err
			}
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgentDirect(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ParentMessageID: "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "test",
			SessionTitle:    "Test",
		})
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parentSession.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.05, updated.Cost, 1e-9)
	})
}

func TestTaskGraphResultMetadataUsesShortTaskRefs(t *testing.T) {
	t.Parallel()

	result := withSubagentOutputMetadata(subagentResult{
		Task: subagentTask{
			Name:        "Review the auth provider and document every observed issue in detail",
			Description: "Review auth",
		},
		TaskRef:        SubagentTaskRef(0, "Review the auth provider and document every observed issue in detail", ""),
		Status:         message.ToolResultSubtaskStatusCompleted,
		ChildSessionID: "child-session-1",
		Content:        strings.Repeat("x", subagentOutputPerTaskCharsLimit+200),
	})

	child := reduceResultToChildSession(result)
	require.True(t, strings.HasPrefix(child.TaskRef, "0-review-the-auth-provider"))
	require.Equal(t, "child-session-1", child.SessionID)
	require.True(t, child.HasFullOutput)
	require.NotEmpty(t, child.Preview)
	require.Equal(t, subagentOutputPreviewCharsLimit, len([]rune(child.Preview)))

	details := subagentOutputDetailsForModel([]subagentResult{result})
	require.Contains(t, details, "full output: subtask://"+child.TaskRef)
	require.Contains(t, details, "Review auth")
}

func TestShortToolCallPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		toolCallID string
		want       string
	}{
		{"empty string", "", ""},
		{"whitespace only", "   ", ""},
		{"normal call_ prefix", "call_96be1a2b", "96be1a"},
		{"short id after prefix", "call_1", "1"},
		{"no call_ prefix long", "abc123def456", "abc123"},
		{"no call_ prefix short", "ab", "ab"},
		{"medium id after prefix", "call_xy", "xy"},
		{"trims whitespace", "  call_96be1a2b  ", "96be1a"},
		{"bare call_", "call_", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, ShortToolCallPrefix(tt.toolCallID))
		})
	}
}

func TestSubagentTaskRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		index      int
		taskID     string
		toolCallID string
		want       string
	}{
		{"no toolCallID", 0, "Review auth", "", "0-review-auth"},
		{"with toolCallID", 1, "Fix bug", "call_96be1a2b", "96be1a-1-fix-bug"},
		{"empty slug fallback", 0, "", "call_abc123", "abc123-0-task"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, SubagentTaskRef(tt.index, tt.taskID, tt.toolCallID))
		})
	}
}

func TestCollectTaskGraphArtifactsExtractsFilesAndShells(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	env := testEnv(t)
	coord := newTestCoordinator(t, env, providerID, providerCfg)

	childSession, err := env.sessions.Create(t.Context(), "Child")
	require.NoError(t, err)

	writeMeta, err := json.Marshal(agenttools.WriteResponseMetadata{
		FilePath:  "/tmp/example.go",
		Diff:      "diff",
		Additions: 3,
		Removals:  1,
	})
	require.NoError(t, err)
	bashMeta, err := json.Marshal(agenttools.BashResponseMetadata{
		ShellID:          "shell-1",
		Description:      "run tests",
		WorkingDirectory: "/tmp",
	})
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), childSession.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "tool-1",
				Name:       agenttools.WriteToolName,
				Metadata:   string(writeMeta),
			},
			message.ToolResult{
				ToolCallID: "tool-2",
				Name:       agenttools.BashToolName,
				Metadata:   string(bashMeta),
			}.WithReducer(message.ToolResultReducer{
				PatchPlan:         []string{"apply generated patch"},
				TestResults:       []string{"go test ./... ok"},
				FollowupQuestions: []string{"Need smoke test on staging?"},
			}),
		},
	})
	require.NoError(t, err)

	artifacts, filesTouched, patchPlan, testResults, followups := coord.collectSubagentArtifacts(t.Context(), childSession.ID)
	require.Equal(t, []string{"file:/tmp/example.go", "shell:shell-1"}, artifacts)
	require.Equal(t, []string{"/tmp/example.go"}, filesTouched)
	require.Equal(t, []string{"apply generated patch"}, patchPlan)
	require.Equal(t, []string{"go test ./... ok"}, testResults)
	require.Equal(t, []string{"Need smoke test on staging?"}, followups)
}

func TestPrepareModelSwitch(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{
		ID: providerID,
		Models: []config.ProviderModel{
			{Model: catwalk.Model{ID: "big", Name: "Big", ContextWindow: 1_000_000, DefaultMaxTokens: 32_000}},
			{Model: catwalk.Model{ID: "small", Name: "Small", ContextWindow: 200_000, DefaultMaxTokens: 8_000}},
		},
	}

	t.Run("summarizes before switching to smaller active model", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)
		coord.cfg.Config().Agents[config.AgentCoder] = config.Agent{Name: config.AgentCoder, Model: config.SelectedModelTypeLarge}

		sess, err := env.sessions.Create(t.Context(), "switch")
		require.NoError(t, err)

		estimates := []int64{250_000, 50_000}
		agent := newMockAgent(providerID, 32_000, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})
		agent.model.CatwalkCfg.ContextWindow = 1_000_000
		agent.estimateFunc = func(_ context.Context, sessionID string, model Model) (int64, error) {
			require.Equal(t, sess.ID, sessionID)
			require.Equal(t, "small", model.ModelCfg.Model)
			v := estimates[0]
			estimates = estimates[1:]
			return v, nil
		}
		coord.currentAgent = agent

		err = coord.PrepareModelSwitch(t.Context(), sess.ID, config.SelectedModelTypeLarge, config.SelectedModel{Provider: providerID, Model: "small"})
		require.NoError(t, err)
		require.Len(t, agent.summarized, 1)
	})

	t.Run("fails when summarization cannot shrink session enough", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)
		coord.cfg.Config().Agents[config.AgentCoder] = config.Agent{Name: config.AgentCoder, Model: config.SelectedModelTypeLarge}

		sess, err := env.sessions.Create(t.Context(), "switch")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 32_000, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})
		agent.model.CatwalkCfg.ContextWindow = 1_000_000
		agent.estimateFunc = func(_ context.Context, _ string, _ Model) (int64, error) {
			return 250_000, nil
		}
		coord.currentAgent = agent

		err = coord.PrepareModelSwitch(t.Context(), sess.ID, config.SelectedModelTypeLarge, config.SelectedModel{Provider: providerID, Model: "small"})
		require.ErrorContains(t, err, "still too large")
		require.Len(t, agent.summarized, 1)
	})

	t.Run("ignores inactive model slot switches", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)
		coord.cfg.Config().Agents[config.AgentCoder] = config.Agent{Name: config.AgentCoder, Model: config.SelectedModelTypeLarge}

		sess, err := env.sessions.Create(t.Context(), "switch")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 32_000, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})
		agent.estimateFunc = func(_ context.Context, _ string, _ Model) (int64, error) {
			t.Fatal("estimate should not be called for inactive model slot")
			return 0, nil
		}
		coord.currentAgent = agent

		err = coord.PrepareModelSwitch(t.Context(), sess.ID, config.SelectedModelTypeSmall, config.SelectedModel{Provider: providerID, Model: "small"})
		require.NoError(t, err)
		require.Empty(t, agent.summarized)
	})
}

func TestBuildAgentModels_ContextWindowOverride(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	providerID := "openai"
	providerCfg := config.ProviderConfig{
		ID:   providerID,
		Type: catwalk.TypeOpenAICompat,
		Models: []config.ProviderModel{
			{Model: catwalk.Model{ID: "big", Name: "Big", ContextWindow: 200_000, DefaultMaxTokens: 32_000}},
			{Model: catwalk.Model{ID: "small", Name: "Small", ContextWindow: 128_000, DefaultMaxTokens: 8_000}},
			{Model: catwalk.Model{ID: "review", Name: "Review", ContextWindow: 512_000, DefaultMaxTokens: 16_000}},
		},
	}
	cfg.Config().Providers.Set(providerID, providerCfg)
	cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
		Provider:        providerID,
		Model:           "big",
		MaxTokens:       32_000,
		ContextWindow:   400_000,
		MaxPromptTokens: 262_144,
	}
	cfg.Config().Models[config.SelectedModelTypeSmall] = config.SelectedModel{
		Provider:  providerID,
		Model:     "small",
		MaxTokens: 8_000,
		// No override for small model.
	}
	cfg.Config().Models[config.SelectedModelTypeReview] = config.SelectedModel{
		Provider: providerID,
		Model:    "review",
	}

	coord := &coordinator{cfg: cfg}
	large, small, err := coord.buildAgentModels(t.Context(), false)
	require.NoError(t, err)
	require.Equal(t, int64(400_000), large.CatwalkCfg.ContextWindow)
	require.Equal(t, int64(128_000), small.CatwalkCfg.ContextWindow)
	require.Equal(t, int64(262_144), large.CatwalkCfg.Options.ProviderOptions["max_prompt_tokens"])

	review, providerCfg, err := coord.resolveAgentInferenceModel(t.Context(), config.Agent{Model: config.SelectedModelTypeReview}, true, large, small)
	require.NoError(t, err)
	require.Equal(t, providerID, providerCfg.ID)
	require.Equal(t, "review", review.ModelCfg.Model)
	require.Equal(t, int64(512_000), review.CatwalkCfg.ContextWindow)
}

func TestFilterAttachmentsForModelSupport(t *testing.T) {
	t.Run("keeps all attachments when model supports images", func(t *testing.T) {
		attachments := []message.Attachment{
			{FilePath: "a.txt", MimeType: "text/plain", Content: []byte("hello")},
			{FilePath: "a.png", MimeType: "image/png", Content: []byte{1, 2, 3}},
		}

		filtered := filterAttachmentsForModelSupport(attachments, true)
		require.Equal(t, attachments, filtered)
	})

	t.Run("filters out non-text attachments when model does not support images", func(t *testing.T) {
		attachments := []message.Attachment{
			{FilePath: "a.txt", MimeType: "text/plain", Content: []byte("hello")},
			{FilePath: "a.png", MimeType: "image/png", Content: []byte{1, 2, 3}},
			{FilePath: "a.jpg", MimeType: "image/jpeg", Content: []byte{4, 5, 6}},
		}

		filtered := filterAttachmentsForModelSupport(attachments, false)
		require.Equal(t, []message.Attachment{attachments[0]}, filtered)
	})

	t.Run("returns nil when attachments are nil", func(t *testing.T) {
		var attachments []message.Attachment
		require.Nil(t, filterAttachmentsForModelSupport(attachments, false))
	})
}

func TestResolveCoderModelSupportsImages(t *testing.T) {
	t.Run("returns image support flag from configured coder model", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, "test-provider", config.ProviderConfig{
			ID: "test-provider",
			Models: []config.ProviderModel{
				{Model: catwalk.Model{ID: "vision-model", SupportsImages: true}},
			},
		})
		coord.cfg.Config().Agents[config.AgentCoder] = config.Agent{Model: config.SelectedModelTypeLarge}
		coord.cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
			Provider: "test-provider",
			Model:    "vision-model",
		}

		supportsImages, err := coord.resolveCoderModelSupportsImages()
		require.NoError(t, err)
		require.True(t, supportsImages)
	})

	t.Run("returns false when configured model does not support images", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, "test-provider", config.ProviderConfig{
			ID: "test-provider",
			Models: []config.ProviderModel{
				{Model: catwalk.Model{ID: "text-model", SupportsImages: false}},
			},
		})
		coord.cfg.Config().Agents[config.AgentCoder] = config.Agent{Model: config.SelectedModelTypeLarge}
		coord.cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
			Provider: "test-provider",
			Model:    "text-model",
		}

		supportsImages, err := coord.resolveCoderModelSupportsImages()
		require.NoError(t, err)
		require.False(t, supportsImages)
	})

	t.Run("returns error when selected model is missing from provider config", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		coord := newTestCoordinator(t, env, "test-provider", config.ProviderConfig{
			ID:     "test-provider",
			Models: []config.ProviderModel{{Model: catwalk.Model{ID: "other-model", SupportsImages: false}}},
		})
		coord.cfg.Config().Agents[config.AgentCoder] = config.Agent{Model: config.SelectedModelTypeLarge}
		coord.cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
			Provider: "test-provider",
			Model:    "missing-model",
		}

		_, err := coord.resolveCoderModelSupportsImages()
		require.ErrorContains(t, err, "model \"missing-model\" not found")
	})

	t.Run("falls back to FindModelInAnyProvider when model is unlisted in selected provider", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, "primary-provider", config.ProviderConfig{
			ID: "primary-provider",
			Models: []config.ProviderModel{
				{Model: catwalk.Model{ID: "other-model", SupportsImages: false}},
			},
		})
		coord.cfg.Config().Providers.Set("catalog-provider", config.ProviderConfig{
			ID: "catalog-provider",
			Models: []config.ProviderModel{
				{Model: catwalk.Model{ID: "vision-model", SupportsImages: true}},
			},
		})
		coord.cfg.Config().Agents[config.AgentCoder] = config.Agent{Model: config.SelectedModelTypeLarge}
		coord.cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
			Provider: "primary-provider",
			Model:    "vision-model",
		}

		supportsImages, err := coord.resolveCoderModelSupportsImages()
		require.NoError(t, err)
		require.True(t, supportsImages)
	})
}

func TestMissingFinishPolicyAllowsJSONFallback(t *testing.T) {
	t.Parallel()

	require.True(t, missingFinishPolicyAllowsJSONFallback(MissingFinishWarn))
	require.True(t, missingFinishPolicyAllowsJSONFallback(MissingFinishRetryThenWarn))
	require.False(t, missingFinishPolicyAllowsJSONFallback(MissingFinishFail))
	require.False(t, missingFinishPolicyAllowsJSONFallback(MissingFinishRetryThenFail))
	require.False(t, missingFinishPolicyAllowsJSONFallback(""))
}

func TestCreateSubagentWorktreeDirUsesProjectDataDir(t *testing.T) {
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("test\n"), 0o644))
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "commit", "-m", "init")

	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(t.TempDir(), "global-data"))
	cfg, err := config.Init(repoDir, "", false)
	require.NoError(t, err)

	coord := &coordinator{cfg: cfg}
	worktreeDir, err := coord.createSubagentWorktreeDir(repoDir, "session/abc$$123")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, coord.removeSubagentWorktree(worktreeDir))
	})

	expectedRoot := filepath.Join(cfg.ProjectDataDir(), "worktrees")
	require.True(t, strings.HasPrefix(worktreeDir, expectedRoot+string(os.PathSeparator)), "worktree %q should be under %q", worktreeDir, expectedRoot)
	require.DirExists(t, worktreeDir)
	require.NoDirExists(t, filepath.Join(repoDir, ".crush", "worktrees"))
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmdArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", cmdArgs...)
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s failed: %s", strings.Join(args, " "), string(output))
}

func TestUpdateParentSessionCost(t *testing.T) {
	t.Run("accumulates cost correctly", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		// Set child cost.
		child.Cost = 0.10
		_, err = env.sessions.Save(t.Context(), child)
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.10, updated.Cost, 1e-9)
	})

	t.Run("accumulates multiple child costs", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		child1, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child1")
		require.NoError(t, err)
		child1.Cost = 0.05
		_, err = env.sessions.Save(t.Context(), child1)
		require.NoError(t, err)

		child2, err := env.sessions.CreateTaskSession(t.Context(), "tool-2", parent.ID, "Child2")
		require.NoError(t, err)
		child2.Cost = 0.03
		_, err = env.sessions.Save(t.Context(), child2)
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child1.ID, parent.ID)
		require.NoError(t, err)
		err = coord.updateParentSessionCost(t.Context(), child2.ID, parent.ID)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.08, updated.Cost, 1e-9)
	})

	t.Run("child session not found", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), "non-existent", parent.ID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get child session")
	})

	t.Run("parent session not found", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, "non-existent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get parent session")
	})

	t.Run("zero cost handled correctly", func(t *testing.T) {
		t.Parallel()

		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.0, updated.Cost, 1e-9)
	})
}

func TestMergeCallOptions_AnthropicThinkingCompatibility(t *testing.T) {
	t.Run("claude 4.6 uses effort without budget thinking", func(t *testing.T) {
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:                     "claude-sonnet-4.6",
				CanReason:              true,
				DefaultReasoningEffort: "high",
				ReasoningLevels:        []string{"high"},
			},
		}
		cfg := config.ProviderConfig{
			Type: anthropic.Name,
		}

		options, _, _, _, _, _ := mergeCallOptions(model, cfg)
		anthropicOpts, ok := options[anthropic.Name].(*anthropic.ProviderOptions)
		require.True(t, ok)
		require.NotNil(t, anthropicOpts)
		require.Equal(t, anthropic.Effort("high"), *anthropicOpts.Effort)
		// Claude 4.6 with effort uses adaptive thinking (SDK handles this)
		require.Nil(t, anthropicOpts.Thinking)
	})

	t.Run("claude opus 4-7 uses effort without budget thinking", func(t *testing.T) {
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:                     "claude-opus-4-7",
				CanReason:              true,
				DefaultReasoningEffort: "high",
				ReasoningLevels:        []string{"high"},
			},
		}
		cfg := config.ProviderConfig{
			Type: anthropic.Name,
		}

		options, _, _, _, _, _ := mergeCallOptions(model, cfg)
		anthropicOpts, ok := options[anthropic.Name].(*anthropic.ProviderOptions)
		require.True(t, ok)
		require.NotNil(t, anthropicOpts)
		require.Equal(t, anthropic.Effort("high"), *anthropicOpts.Effort)
		require.Nil(t, anthropicOpts.Thinking)
	})

	t.Run("claude opus 4.7 canReason enables effort by default", func(t *testing.T) {
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:              "claude-opus-4.7",
				CanReason:       true,
				ReasoningLevels: []string{"high"},
			},
		}
		cfg := config.ProviderConfig{
			Type: anthropic.Name,
		}

		options, _, _, _, _, _ := mergeCallOptions(model, cfg)
		anthropicOpts, ok := options[anthropic.Name].(*anthropic.ProviderOptions)
		require.True(t, ok)
		require.NotNil(t, anthropicOpts)
		require.Equal(t, anthropic.Effort("high"), *anthropicOpts.Effort)
		require.Nil(t, anthropicOpts.Thinking)
	})

	t.Run("claude opus 4.6 with think flag uses high effort", func(t *testing.T) {
		think := true
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:              "claude-opus-4-6",
				CanReason:       true,
				ReasoningLevels: []string{"high"},
			},
			ModelCfg: config.SelectedModel{
				Think: &think,
			},
		}
		cfg := config.ProviderConfig{
			Type: anthropic.Name,
		}

		options, _, _, _, _, _ := mergeCallOptions(model, cfg)
		anthropicOpts, ok := options[anthropic.Name].(*anthropic.ProviderOptions)
		require.True(t, ok)
		require.NotNil(t, anthropicOpts)
		require.Equal(t, anthropic.Effort("high"), *anthropicOpts.Effort)
		require.Nil(t, anthropicOpts.Thinking)
	})

	t.Run("older claude uses budget thinking without effort", func(t *testing.T) {
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:                     "claude-sonnet-4",
				CanReason:              true,
				DefaultReasoningEffort: "high",
			},
		}
		cfg := config.ProviderConfig{
			Type: anthropic.Name,
		}

		options, _, _, _, _, _ := mergeCallOptions(model, cfg)
		anthropicOpts, ok := options[anthropic.Name].(*anthropic.ProviderOptions)
		require.True(t, ok)
		require.NotNil(t, anthropicOpts)
		require.Nil(t, anthropicOpts.Effort)
		require.NotNil(t, anthropicOpts.Thinking)
		require.Equal(t, int64(28672), anthropicOpts.Thinking.BudgetTokens)
	})

	t.Run("canReason model enables thinking by default", func(t *testing.T) {
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:        "claude-sonnet-4",
				CanReason: true,
			},
		}
		cfg := config.ProviderConfig{
			Type: anthropic.Name,
		}

		options, _, _, _, _, _ := mergeCallOptions(model, cfg)
		anthropicOpts, ok := options[anthropic.Name].(*anthropic.ProviderOptions)
		require.True(t, ok)
		require.NotNil(t, anthropicOpts)
		require.Nil(t, anthropicOpts.Effort)
		require.NotNil(t, anthropicOpts.Thinking)
		require.Equal(t, int64(28672), anthropicOpts.Thinking.BudgetTokens)
	})

	t.Run("thinking is disabled when Think is explicitly false", func(t *testing.T) {
		think := false
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:        "claude-sonnet-4",
				CanReason: true,
			},
			ModelCfg: config.SelectedModel{
				Think: &think,
			},
		}
		cfg := config.ProviderConfig{
			Type: anthropic.Name,
		}

		options, _, _, _, _, _ := mergeCallOptions(model, cfg)
		anthropicOpts, ok := options[anthropic.Name].(*anthropic.ProviderOptions)
		require.True(t, ok)
		require.NotNil(t, anthropicOpts)
		require.Nil(t, anthropicOpts.Effort)
		require.Nil(t, anthropicOpts.Thinking)
	})

	t.Run("thinking is disabled for claude 4.6 when Think is explicitly false", func(t *testing.T) {
		think := false
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:              "claude-sonnet-4.6",
				CanReason:       true,
				ReasoningLevels: []string{"high"},
			},
			ModelCfg: config.SelectedModel{
				Think: &think,
			},
		}
		cfg := config.ProviderConfig{
			Type: anthropic.Name,
		}

		options, _, _, _, _, _ := mergeCallOptions(model, cfg)
		anthropicOpts, ok := options[anthropic.Name].(*anthropic.ProviderOptions)
		require.True(t, ok)
		require.NotNil(t, anthropicOpts)
		require.Nil(t, anthropicOpts.Effort)
		require.Nil(t, anthropicOpts.Thinking)
	})

	t.Run("isAnthropicThinking returns true for any CanReason model", func(t *testing.T) {
		for _, id := range []string{"claude-sonnet-4", "claude-sonnet-4.6", "kimi-k2.5"} {
			require.True(t, isAnthropicThinking(catwalk.Model{ID: id, CanReason: true}), id)
		}
	})

	t.Run("isAnthropicThinking returns false when CanReason is false", func(t *testing.T) {
		require.False(t, isAnthropicThinking(catwalk.Model{ID: "claude-sonnet-4", CanReason: false}))
	})

	t.Run("claude 4.6 canReason enables effort by default", func(t *testing.T) {
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:        "claude-sonnet-4.6",
				CanReason: true,
			},
		}
		cfg := config.ProviderConfig{
			Type: anthropic.Name,
		}

		options, _, _, _, _, _ := mergeCallOptions(model, cfg)
		anthropicOpts, ok := options[anthropic.Name].(*anthropic.ProviderOptions)
		require.True(t, ok)
		require.NotNil(t, anthropicOpts)
		require.Equal(t, anthropic.Effort("high"), *anthropicOpts.Effort)
		require.Nil(t, anthropicOpts.Thinking)
	})
}

func TestMergeCallOptions_ThinkDisabledAllProviders(t *testing.T) {
	t.Parallel()

	think := false

	t.Run("openai: no reasoning_effort when Think is false", func(t *testing.T) {
		t.Parallel()
		// Use a non-responses model ID so mergeCallOptions returns *ProviderOptions.
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:        "custom-reasoning-model",
				CanReason: true,
			},
			ModelCfg: config.SelectedModel{Think: &think},
		}
		options, _, _, _, _, _ := mergeCallOptions(model, config.ProviderConfig{Type: openai.Name})
		opts, ok := options[openai.Name].(*openai.ProviderOptions)
		require.True(t, ok)
		require.Nil(t, opts.ReasoningEffort)
	})

	t.Run("openai: reasoning_effort set by default when Think is nil", func(t *testing.T) {
		t.Parallel()
		// Use a non-responses model ID so mergeCallOptions returns *ProviderOptions.
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:        "custom-reasoning-model",
				CanReason: true,
			},
		}
		options, _, _, _, _, _ := mergeCallOptions(model, config.ProviderConfig{Type: openai.Name})
		opts, ok := options[openai.Name].(*openai.ProviderOptions)
		require.True(t, ok)
		require.NotNil(t, opts.ReasoningEffort)
		require.Equal(t, openai.ReasoningEffortHigh, *opts.ReasoningEffort)
	})

	t.Run("openai: use_responses_api forces Responses provider options", func(t *testing.T) {
		t.Parallel()

		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:        "grok-composer-2.5-fast",
				CanReason: true,
			},
		}
		providerCfg := config.ProviderConfig{
			Type: openai.Name,
			Models: []config.ProviderModel{{
				Model: catwalk.Model{
					ID: "grok-composer-2.5-fast",
				},
				UseResponsesAPI: true,
			}},
		}
		options, _, _, _, _, _ := mergeCallOptions(model, providerCfg)
		_, ok := options[openai.Name].(*openai.ResponsesProviderOptions)
		require.True(t, ok)
	})

	t.Run("openai-compat: no reasoning_effort when Think is false", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:        "some-reasoning-model",
				CanReason: true,
			},
			ModelCfg: config.SelectedModel{Think: &think},
		}
		options, _, _, _, _, _ := mergeCallOptions(model, config.ProviderConfig{Type: openaicompat.Name})
		opts, ok := options[openaicompat.Name].(*openaicompat.ProviderOptions)
		require.True(t, ok)
		require.Nil(t, opts.ReasoningEffort)
	})
}

func TestMergeCallOptions_ThinkDisabledClearsProviderOptions(t *testing.T) {
	t.Parallel()

	think := false

	t.Run("anthropic: Think=false clears effort set in provider config", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:        "claude-sonnet-4",
				CanReason: true,
			},
			ModelCfg: config.SelectedModel{Think: &think},
		}
		// Provider config has effort pre-set.
		cfg := config.ProviderConfig{
			Type:            anthropic.Name,
			ProviderOptions: map[string]any{"effort": "high"},
		}

		options, _, _, _, _, _ := mergeCallOptions(model, cfg)
		anthropicOpts, ok := options[anthropic.Name].(*anthropic.ProviderOptions)
		require.True(t, ok)
		require.Nil(t, anthropicOpts.Effort)
		require.Nil(t, anthropicOpts.Thinking)
	})

	t.Run("openai: Think=false clears reasoning_effort set in provider config", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:        "custom-reasoning-model",
				CanReason: true,
			},
			ModelCfg: config.SelectedModel{Think: &think},
		}
		cfg := config.ProviderConfig{
			Type:            openai.Name,
			ProviderOptions: map[string]any{"reasoning_effort": "high"},
		}

		options, _, _, _, _, _ := mergeCallOptions(model, cfg)
		opts, ok := options[openai.Name].(*openai.ProviderOptions)
		require.True(t, ok)
		require.Nil(t, opts.ReasoningEffort)
	})

	t.Run("openai-compat: Think=false clears reasoning_effort set in provider config", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatwalkCfg: catwalk.Model{
				ID:        "custom-reasoning-model",
				CanReason: true,
			},
			ModelCfg: config.SelectedModel{Think: &think},
		}
		cfg := config.ProviderConfig{
			Type:            openaicompat.Name,
			ProviderOptions: map[string]any{"reasoning_effort": "high"},
		}

		options, _, _, _, _, _ := mergeCallOptions(model, cfg)
		opts, ok := options[openaicompat.Name].(*openaicompat.ProviderOptions)
		require.True(t, ok)
		require.Nil(t, opts.ReasoningEffort)
	})
}

func TestWrapOpenAIStreamingHTTPClient(t *testing.T) {
	t.Parallel()

	t.Run("uses websocket wrapper when enabled", func(t *testing.T) {
		t.Parallel()

		wrapped := wrapOpenAIStreamingHTTPClient(nil, true)
		require.NotNil(t, wrapped)

		transportType := reflect.TypeOf(wrapped.Transport).String()
		require.Equal(t, "*httpext.activityTrackingTransport", transportType)

		activityValue := reflect.ValueOf(wrapped.Transport).Elem()
		baseField := activityValue.FieldByName("base")
		require.True(t, baseField.IsValid())
		base := reflect.NewAt(baseField.Type(), unsafe.Pointer(baseField.UnsafeAddr())).Elem().Interface().(http.RoundTripper)
		require.Equal(t, "httpext.openAIResponsesWebSocketTransport", reflect.TypeOf(base).String())
	})

	t.Run("uses activity wrapper only when disabled", func(t *testing.T) {
		t.Parallel()

		wrapped := wrapOpenAIStreamingHTTPClient(nil, false)
		require.NotNil(t, wrapped)

		transportType := reflect.TypeOf(wrapped.Transport).String()
		require.Equal(t, "*httpext.activityTrackingTransport", transportType)

		activityValue := reflect.ValueOf(wrapped.Transport).Elem()
		baseField := activityValue.FieldByName("base")
		require.True(t, baseField.IsValid())
		base := reflect.NewAt(baseField.Type(), unsafe.Pointer(baseField.UnsafeAddr())).Elem().Interface().(http.RoundTripper)
		require.Same(t, http.DefaultTransport, base)
	})
}

func TestBuildProvider_PreservesHyperBaseURL(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, hyper.Name, config.ProviderConfig{
		ID:      hyper.Name,
		Type:    hyper.Name,
		BaseURL: "https://hyper.example.test/api/v1/fantasy",
		APIKey:  "test-key",
	})

	provider, err := coord.buildProvider(config.ProviderConfig{
		ID:      hyper.Name,
		Type:    hyper.Name,
		BaseURL: "https://hyper.example.test/api/v1/fantasy",
		APIKey:  "test-key",
	}, catwalk.Model{}, false, false)
	require.NoError(t, err)
	require.Equal(t, "https://hyper.example.test/api/v1/fantasy", hyperProviderBaseURL(t, provider))
}

func hyperProviderBaseURL(t *testing.T, provider fantasy.Provider) string {
	t.Helper()

	value := reflect.ValueOf(provider)
	require.Equal(t, reflect.Pointer, value.Kind())

	optionsField := value.Elem().FieldByName("options")
	require.True(t, optionsField.IsValid())

	optionsValue := reflect.NewAt(optionsField.Type(), unsafe.Pointer(optionsField.UnsafeAddr())).Elem()
	baseURLField := optionsValue.FieldByName("baseURL")
	require.True(t, baseURLField.IsValid())

	return reflect.NewAt(baseURLField.Type(), unsafe.Pointer(baseURLField.UnsafeAddr())).Elem().String()
}

func TestEnableSessionMemory_BackendAware(t *testing.T) {
	t.Parallel()

	providerCfg := config.ProviderConfig{
		ID:     "test-provider",
		Type:   catwalk.TypeOpenAICompat,
		Models: []config.ProviderModel{config.ProviderModelID("test-model")},
	}

	tests := []struct {
		backend     string
		wantEnabled bool
	}{
		{"local", true},
		{"hindsight", false},
	}

	for _, tt := range tests {
		t.Run(tt.backend, func(t *testing.T) {
			t.Parallel()

			// Each sub-test builds its own coordinator to avoid concurrent
			// writes racing on SetMemoryBackend.
			env := testEnv(t)
			coord := newTestCoordinator(t, env, "test-provider", providerCfg)
			coord.cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
				Provider: "test-provider",
				Model:    "test-model",
			}
			coord.cfg.Config().Models[config.SelectedModelTypeSmall] = config.SelectedModel{
				Provider: "test-provider",
				Model:    "test-model",
			}

			conn, err := db.Connect(t.Context(), t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { conn.Close() })

			eng := engine.New(conn, engine.Config{Enabled: true, Backend: tt.backend})
			var backend memory.Backend
			if tt.backend == "hindsight" {
				backend = memory.NewHindsightBackend(eng, nil, nil)
			} else {
				backend = memory.NewLocalBackend(eng)
			}
			coord.SetMemoryBackend(backend)

			agent, err := coord.buildAgent(t.Context(), nil, config.Agent{}, true)
			require.NoError(t, err)

			sa, ok := agent.(*sessionAgent)
			require.True(t, ok)
			// Verify buildAgent sets EnableSessionMemory based on the
			// backend's SessionWorkingMemory capability. Checking
			// sessionMemoryEnabled directly (rather than
			// enableSessionMemory()) avoids coupling this test to whether a
			// background model is configured.
			assert.Equal(t, tt.wantEnabled, sa.sessionMemoryEnabled)
		})
	}
}

func TestGetProviderOptionsReasoningEffort(t *testing.T) {
	// Bedrock is Fantasy's Anthropic under a different provider name; options
	// must land under anthropic.Name so the Anthropic language model picks them up.
	tests := []struct {
		name         string
		providerType catwalk.Type
	}{
		{"anthropic honors reasoning_effort", catwalk.Type(anthropic.Name)},
		{"bedrock honors reasoning_effort", catwalk.Type(bedrock.Name)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := Model{
				CatwalkCfg: catwalk.Model{
					ID:              "claude-opus-4-7",
					CanReason:       true,
					ReasoningLevels: []string{"max"},
				},
				ModelCfg: config.SelectedModel{
					Provider:        "test",
					ReasoningEffort: "max",
				},
			}
			providerCfg := config.ProviderConfig{ID: "test", Type: tc.providerType}

			opts := getProviderOptions(model, providerCfg)

			raw, ok := opts[anthropic.Name]
			require.True(t, ok, "options should be keyed under anthropic.Name for type %q", tc.providerType)
			parsed, ok := raw.(*anthropic.ProviderOptions)
			require.True(t, ok)
			require.NotNil(t, parsed.Effort)
			assert.Equal(t, anthropic.Effort("max"), *parsed.Effort)
		})
	}
}

func TestTryFallbackPayloadFromOutput(t *testing.T) {
	t.Parallel()

	exploreSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary": map[string]any{"type": "string"},
			"files": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":        map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
					},
					"required": []string{"path", "description"},
				},
			},
		},
		"required": []string{"summary", "files"},
	}

	tests := []struct {
		name    string
		content string
		schema  any
		want    bool
	}{
		{
			name:    "valid JSON conforming to schema",
			content: `{"summary":"found it","files":[{"path":"foo.go","description":"main file"}]}`,
			schema:  exploreSchema,
			want:    true,
		},
		{
			name:    "valid JSON wrapped in markdown fence",
			content: "```json\n{\"summary\":\"found it\",\"files\":[]}\n```",
			schema:  exploreSchema,
			want:    true,
		},
		{
			name:    "invalid JSON",
			content: "This is just text, not JSON.",
			schema:  exploreSchema,
			want:    false,
		},
		{
			name:    "valid JSON but does not conform to schema",
			content: `{"wrong":"field"}`,
			schema:  exploreSchema,
			want:    false,
		},
		{
			name:    "empty content",
			content: "",
			schema:  exploreSchema,
			want:    false,
		},
		{
			name:    "nil schema accepts any valid JSON",
			content: `{"anything":"goes"}`,
			schema:  nil,
			want:    true,
		},
		{
			name:    "nil schema rejects invalid JSON",
			content: "not json",
			schema:  nil,
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := tryFallbackPayloadFromOutput(tc.content, tc.schema)
			if tc.want {
				require.NotNil(t, result, "expected non-nil payload")
				// Verify the result is valid JSON.
				var parsed any
				require.NoError(t, json.Unmarshal(result, &parsed))
			} else {
				require.Nil(t, result, "expected nil payload")
			}
		})
	}
}

func TestBuildSubagentHandoffSummary(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "test", config.ProviderConfig{ID: "test"})

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "parent")
	require.NoError(t, err)

	// Create a user message (original request).
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Please fix the authentication bug in the login handler."},
		},
	})
	require.NoError(t, err)

	// Create an assistant message with thinking and text.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "internal reasoning about auth"},
			message.TextContent{Text: "I found the bug in auth.go line 42. The token validation is missing a nil check."},
			message.ToolCall{Name: "read", Input: `{"path":"internal/auth.go","offset":40,"limit":10}`},
		},
	})
	require.NoError(t, err)

	// Create another assistant message.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "The fix requires adding a nil guard before calling ValidateToken."},
			message.ToolCall{Name: "grep", Input: `{"pattern":"ValidateToken","output_mode":"content"}`},
		},
	})
	require.NoError(t, err)

	result := coord.buildSubagentHandoffSummary(ctx, sess.ID)

	// Verify overall structure.
	assert.Contains(t, result, "<parent_context>")
	assert.Contains(t, result, "</parent_context>")

	// Verify original request section.
	assert.Contains(t, result, "<original_request>")
	assert.Contains(t, result, "authentication bug")
	assert.Contains(t, result, "</original_request>")

	// Verify recent reasoning section.
	assert.Contains(t, result, "<recent_reasoning>")
	assert.Contains(t, result, "nil guard")
	assert.Contains(t, result, "</recent_reasoning>")

	// Verify thinking content is stripped.
	assert.NotContains(t, result, "internal reasoning about auth")

	// Verify key actions section.
	assert.Contains(t, result, "<key_actions>")
	assert.Contains(t, result, "read(")
	assert.Contains(t, result, "grep(")
	assert.Contains(t, result, "</key_actions>")
}

func TestBuildSubagentHandoffSummary_Empty(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "test", config.ProviderConfig{ID: "test"})

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "empty-parent")
	require.NoError(t, err)

	// No messages → returns "".
	result := coord.buildSubagentHandoffSummary(ctx, sess.ID)
	assert.Equal(t, "", result)
}

func TestBuildSubagentHandoffSummary_ThinkingStripped(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "test", config.ProviderConfig{ID: "test"})

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "thinking-parent")
	require.NoError(t, err)

	// Create an assistant message with only thinking content (no visible text).
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "secret internal chain of thought"},
			message.TextContent{Text: "<think>secret internal chain of thought</think>"},
		},
	})
	require.NoError(t, err)

	result := coord.buildSubagentHandoffSummary(ctx, sess.ID)

	// Thinking content should not appear.
	assert.NotContains(t, result, "secret internal chain")
	// With no visible text and no user messages, result should be empty.
	assert.Equal(t, "", result)
}

func TestAssembleSubagentPrompt(t *testing.T) {
	t.Parallel()

	t.Run("both contexts present", func(t *testing.T) {
		t.Parallel()
		parentCtx := "<parent_context>\nSome auto-summary\n</parent_context>"
		sharedCtx := "Shared background from parent agent."
		assignment := "Fix the bug."

		result := assembleSubagentPrompt(parentCtx, sharedCtx, assignment)

		assert.Contains(t, result, "<parent_context>")
		assert.Contains(t, result, "Some auto-summary")
		assert.Contains(t, result, "<shared_context>")
		assert.Contains(t, result, "Shared background from parent agent.")
		assert.Contains(t, result, "</shared_context>")
		assert.Contains(t, result, "</parent_context>")
		assert.Contains(t, result, "Fix the bug.")
		// Verify assignment comes after the context block.
		assert.Greater(t, strings.Index(result, "Fix the bug."), strings.Index(result, "</parent_context>"))
	})

	t.Run("only parent context", func(t *testing.T) {
		t.Parallel()
		result := assembleSubagentPrompt("auto-summary", "", "Do work.")
		assert.Contains(t, result, "<parent_context>")
		assert.Contains(t, result, "auto-summary")
		assert.NotContains(t, result, "<shared_context>")
		assert.Contains(t, result, "Do work.")
	})

	t.Run("only shared context", func(t *testing.T) {
		t.Parallel()
		result := assembleSubagentPrompt("", "shared info", "Do work.")
		assert.Contains(t, result, "<parent_context>")
		assert.Contains(t, result, "<shared_context>")
		assert.Contains(t, result, "shared info")
		assert.Contains(t, result, "Do work.")
	})

	t.Run("both empty", func(t *testing.T) {
		t.Parallel()
		result := assembleSubagentPrompt("", "", "Just the assignment.")
		assert.Equal(t, "Just the assignment.", result)
		assert.NotContains(t, result, "<parent_context>")
	})
}

func TestWriteParentHistoryFile(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "test", config.ProviderConfig{ID: "test"})

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "history-parent")
	require.NoError(t, err)

	// Create messages.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "Build a REST API."}},
	})
	require.NoError(t, err)

	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "I'll create the API endpoints now."},
			message.ToolCall{Name: "write", Input: `{"path":"api/handlers.go"}`},
		},
	})
	require.NoError(t, err)

	historyFile := coord.writeParentHistoryFile(ctx, sess.ID)

	if historyFile == "" {
		t.Skip("history file not created (project data dir unavailable)")
	}
	defer coord.cleanupParentHistoryFile(historyFile)

	// Verify file exists and has content.
	data, readErr := os.ReadFile(historyFile)
	require.NoError(t, readErr)
	content := string(data)

	assert.Contains(t, content, "# Parent Session History")
	assert.Contains(t, content, "[User]")
	assert.Contains(t, content, "REST API")
	assert.Contains(t, content, "[Assistant]")
	assert.Contains(t, content, "API endpoints")
	assert.Contains(t, content, "[ToolCall] write(")

	// Verify cleanup works.
	coord.cleanupParentHistoryFile(historyFile)
	_, statErr := os.Stat(historyFile)
	assert.True(t, os.IsNotExist(statErr), "history file should be removed after cleanup")
}

func TestFormatHistoryLine(t *testing.T) {
	t.Parallel()

	t.Run("user message", func(t *testing.T) {
		t.Parallel()
		msg := message.Message{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "Fix the bug."}},
		}
		line := formatHistoryLine(msg, 200)
		assert.Contains(t, line, "[User]")
		assert.Contains(t, line, "Fix the bug.")
	})

	t.Run("assistant with thinking stripped", func(t *testing.T) {
		t.Parallel()
		msg := message.Message{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "<think>secret thoughts</think>Visible text here."},
			},
		}
		line := formatHistoryLine(msg, 200)
		assert.Contains(t, line, "[Assistant]")
		assert.Contains(t, line, "Visible text here.")
		assert.NotContains(t, line, "secret thoughts")
	})

	t.Run("summary message skipped", func(t *testing.T) {
		t.Parallel()
		msg := message.Message{
			Role:             message.Assistant,
			Parts:            []message.ContentPart{message.TextContent{Text: "summary"}},
			IsSummaryMessage: true,
		}
		line := formatHistoryLine(msg, 200)
		assert.Equal(t, "", line)
	})
}

func TestExtractOriginalRequest(t *testing.T) {
	t.Parallel()

	msgs := []message.Message{
		{Role: message.System, Parts: []message.ContentPart{message.TextContent{Text: "system"}}},
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "First user request."}}},
		{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "response"}}},
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "Second user message."}}},
	}

	result := extractOriginalRequest(msgs, 300)
	assert.Equal(t, "First user request.", result)
}

func TestExtractOriginalRequest_Truncation(t *testing.T) {
	t.Parallel()

	longText := strings.Repeat("a", 500)
	msgs := []message.Message{
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: longText}}},
	}

	result := extractOriginalRequest(msgs, 300)
	assert.True(t, len([]rune(result)) <= 302) // 300 + "…" + possible margin
	assert.Contains(t, result, "…")
}

func TestExtractKeyActions(t *testing.T) {
	t.Parallel()

	msgs := []message.Message{
		{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{Name: "read", Input: `{"path":"foo.go"}`},
				message.ToolCall{Name: "grep", Input: `{"pattern":"bar"}`},
			},
		},
		{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{Name: "bash", Input: `{"command":"go test"}`},
				message.ToolCall{Name: "read", Input: `{"path":"bar.go"}`}, // duplicate name, this one is more recent
			},
		},
	}

	actions := extractKeyActions(msgs, 5, 80)
	require.Len(t, actions, 3) // read, grep, bash (read deduplicated, keeping the more recent)
	// The most recent read should reference bar.go (at index 0 since forward
	// iteration replaced the older read(foo.go) entry in-place).
	assert.Contains(t, actions[0], "bar.go")
}

func TestFormatToolCallSummary(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "read", formatToolCallSummary("read", "", 80))
	assert.Contains(t, formatToolCallSummary("bash", `{"command":"go test ./..."}`, 80), "bash(")
	assert.Contains(t, formatToolCallSummary("bash", `{"command":"go test ./..."}`, 80), "go test ./...")

	longInput := `{"command":"` + strings.Repeat("x", 200) + `"}`
	result := formatToolCallSummary("bash", longInput, 80)
	assert.Contains(t, result, "…")
	assert.True(t, len([]rune(result)) < 100)
}
