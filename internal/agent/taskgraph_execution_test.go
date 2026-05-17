package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/mailbox"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

type subagentMockSessionAgent struct {
	model         Model
	runFunc       func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)
	cancelAllFunc func()
}

func (m *subagentMockSessionAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	if m.runFunc != nil {
		return m.runFunc(ctx, call)
	}
	return &fantasy.AgentResult{}, nil
}

func (m *subagentMockSessionAgent) EstimateSessionPromptTokensForModel(context.Context, string, Model) (int64, error) {
	return 0, nil
}

func (m *subagentMockSessionAgent) Model() Model                                    { return m.model }
func (m *subagentMockSessionAgent) SetModels(large, small Model)                    {}
func (m *subagentMockSessionAgent) SetTools(tools []fantasy.AgentTool)              {}
func (m *subagentMockSessionAgent) SetSystemPrompt(systemPrompt string)             {}
func (m *subagentMockSessionAgent) SetSystemPromptPrefix(systemPromptPrefix string) {}
func (m *subagentMockSessionAgent) Cancel(sessionID string)                         {}
func (m *subagentMockSessionAgent) CancelAll() {
	if m.cancelAllFunc != nil {
		m.cancelAllFunc()
	}
}
func (m *subagentMockSessionAgent) IsSessionBusy(sessionID string) bool         { return false }
func (m *subagentMockSessionAgent) IsBusy() bool                                { return false }
func (m *subagentMockSessionAgent) QueuedPrompts(sessionID string) int          { return 0 }
func (m *subagentMockSessionAgent) QueuedPromptsList(sessionID string) []string { return nil }
func (m *subagentMockSessionAgent) RemoveQueuedPrompt(sessionID string, index int) bool {
	return false
}
func (m *subagentMockSessionAgent) ClearQueue(sessionID string)  {}
func (m *subagentMockSessionAgent) PauseQueue(sessionID string)  {}
func (m *subagentMockSessionAgent) ResumeQueue(sessionID string) {}
func (m *subagentMockSessionAgent) IsQueuePaused(sessionID string) bool {
	return false
}

func (m *subagentMockSessionAgent) PrioritizeQueuedPrompt(sessionID string, index int) bool {
	return false
}

func (m *subagentMockSessionAgent) Summarize(context.Context, string, fantasy.ProviderOptions) error {
	return nil
}

func (m *subagentMockSessionAgent) RespondAsBackground(_ context.Context, _, _ string) (string, error) {
	return "mock irc reply", nil
}

func TestRunSubagents_ParallelExecution(t *testing.T) {
	env := testEnv(t)
	rootSession, err := env.sessions.Create(context.Background(), "subagent-parallel")
	require.NoError(t, err)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	coord := &coordinator{cfg: cfg, mailbox: mailbox.NewService(), sessions: env.sessions, agentRegistry: GlobalAgentRegistry()}

	var running int32
	var maxRunning int32
	var runMu sync.Mutex
	runOrder := make([]string, 0)

	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		taskName := requestedType
		agent := &subagentMockSessionAgent{
			model: Model{
				CatwalkCfg: catwalk.Model{DefaultMaxTokens: 1000},
				ModelCfg:   config.SelectedModel{Provider: "test-provider", Model: "test-model"},
			},
			runFunc: func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
				current := atomic.AddInt32(&running, 1)
				for {
					prev := atomic.LoadInt32(&maxRunning)
					if current <= prev {
						break
					}
					if atomic.CompareAndSwapInt32(&maxRunning, prev, current) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				runMu.Lock()
				runOrder = append(runOrder, taskName)
				runMu.Unlock()
				atomic.AddInt32(&running, -1)
				return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "ok"}}}}, nil
			},
		}
		return agent, config.Agent{ID: taskName, Description: taskName, Mode: config.AgentModeSubagent}, nil
	}

	coord.subAgentScheduler = func(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
		_, err := params.Agent.Run(ctx, SessionAgentCall{Prompt: params.Prompt})
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return withSubtaskToolResponseMetadata(
			fantasy.NewTextResponse("done"),
			params.ToolCallID,
			params.AgentMessageID+"$$"+params.ToolCallID,
			params.ParentMessageID,
			message.ToolResultSubtaskStatusCompleted,
		), nil
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = coord.mailbox.Send("call-1", "c", "prioritize final checks")
	}()

	resp, err := coord.runSubagents(t.Context(), subagentBatchParams{
		SessionID:      rootSession.ID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Tasks: []subagentTask{
			{Name: "a", Assignment: "task-a", SubagentType: "a"},
			{Name: "b", Assignment: "task-b", SubagentType: "b"},
			{Name: "c", Assignment: "task-c", SubagentType: "c"},
		},
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.GreaterOrEqual(t, atomic.LoadInt32(&maxRunning), int32(2))
	require.Contains(t, resp.Metadata, "mailbox_id")
	require.Contains(t, resp.Metadata, "child_sessions")
	require.Contains(t, resp.Content, "Child sessions:")
	require.Contains(t, resp.Content, "a (completed)")
	require.Contains(t, resp.Content, "b (completed)")
	require.Contains(t, resp.Content, "c (completed)")
	require.Contains(t, resp.Content, "Task outputs:")
	require.Contains(t, resp.Content, message.SanitizedToolResultStub)

	loadedSession, err := env.sessions.Get(context.Background(), rootSession.ID)
	require.NoError(t, err)
	require.Len(t, loadedSession.Todos, 3)

	runMu.Lock()
	defer runMu.Unlock()
	require.Len(t, runOrder, 3)

	reducerMeta, hasReducer := message.ParseToolResultReducer(resp.Metadata)
	require.True(t, hasReducer)
	require.Empty(t, reducerMeta.PatchPlan)
	require.Empty(t, reducerMeta.TestResults)
	require.Empty(t, reducerMeta.FollowupQuestions)
}

func TestRunSubagents_SingleTaskKeepsSubtaskMetadata(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	coord := &coordinator{cfg: cfg, mailbox: mailbox.NewService(), sessions: env.sessions, agentRegistry: GlobalAgentRegistry()}

	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		agent := &subagentMockSessionAgent{
			model: Model{CatwalkCfg: catwalk.Model{DefaultMaxTokens: 1000}, ModelCfg: config.SelectedModel{Provider: "test-provider", Model: "test-model"}},
		}
		return agent, config.Agent{ID: requestedType, Description: requestedType, Mode: config.AgentModeSubagent}, nil
	}
	coord.subAgentScheduler = func(_ context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
		resp := withSubtaskToolResponseMetadata(
			fantasy.NewTextResponse("ok"),
			params.ToolCallID,
			"child-session-1",
			params.ParentMessageID,
			message.ToolResultSubtaskStatusCompletedWithWarnings,
		)
		resp = withSubagentFinishToolResponseMetadata(resp, message.ToolResultSubagentFinish{
			Status:      message.ToolResultSubtaskStatusCompletedWithWarnings,
			Summary:     "ok",
			Artifacts:   []string{"artifact.txt"},
			PatchPlan:   []string{"update"},
			TestResults: []string{"go test ./..."},
			Followups:   []string{"review"},
			Error:       "minor warning",
		})
		return resp, nil
	}

	resp, err := coord.runSubagents(t.Context(), subagentBatchParams{
		SessionID:      "session-1",
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Tasks: []subagentTask{
			{Name: "only", Assignment: "only", SubagentType: "general"},
		},
	})
	require.NoError(t, err)
	subtask, ok := message.ParseToolResultSubtaskResult(resp.Metadata)
	require.True(t, ok)
	require.Equal(t, "child-session-1", subtask.ChildSessionID)
	require.Equal(t, message.ToolResultSubtaskStatusCompletedWithWarnings, subtask.Status)
	finish, ok := message.ParseToolResultSubagentFinish(resp.Metadata)
	require.True(t, ok)
	require.Equal(t, message.ToolResultSubtaskStatusCompletedWithWarnings, finish.Status)
	reducerMeta, hasReducer := message.ParseToolResultReducer(resp.Metadata)
	require.True(t, hasReducer)
	require.Equal(t, "high", reducerMeta.Confidence)
}

func TestRunSubagents_HonorsMaxConcurrentPerAgent(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	maxConcurrent := 1
	cfg.Config().Agents[config.AgentGeneral] = config.Agent{
		ID:          config.AgentGeneral,
		Description: "general",
		Mode:        config.AgentModeSubagent,
		TaskGovernance: &config.TaskGovernance{
			MaxConcurrent: &maxConcurrent,
		},
	}
	coord := &coordinator{cfg: cfg, mailbox: mailbox.NewService(), sessions: env.sessions, agentRegistry: GlobalAgentRegistry()}

	var running int32
	var maxRunning int32
	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		agent := &subagentMockSessionAgent{
			model: Model{CatwalkCfg: catwalk.Model{DefaultMaxTokens: 1000}, ModelCfg: config.SelectedModel{Provider: "test-provider", Model: "test-model"}},
			runFunc: func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
				current := atomic.AddInt32(&running, 1)
				for {
					prev := atomic.LoadInt32(&maxRunning)
					if current <= prev {
						break
					}
					if atomic.CompareAndSwapInt32(&maxRunning, prev, current) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				atomic.AddInt32(&running, -1)
				return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "ok"}}}}, nil
			},
		}
		return agent, cfg.Config().Agents[config.AgentGeneral], nil
	}
	coord.subAgentScheduler = func(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
		_, err := params.Agent.Run(ctx, SessionAgentCall{Prompt: params.Prompt})
		require.NoError(t, err)
		return withSubtaskToolResponseMetadata(fantasy.NewTextResponse("ok"), params.ToolCallID, params.ToolCallID, params.ParentMessageID, message.ToolResultSubtaskStatusCompleted), nil
	}

	resp, err := coord.runSubagents(t.Context(), subagentBatchParams{
		SessionID:      "session-1",
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Tasks: []subagentTask{
			{Name: "a", Assignment: "task-a", SubagentType: "general"},
			{Name: "b", Assignment: "task-b", SubagentType: "general"},
		},
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, int32(1), atomic.LoadInt32(&maxRunning))
}

func TestRunSubagents_SkipsPerChildHandoffReview(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Agents[config.AgentGeneral] = config.Agent{
		ID:          config.AgentGeneral,
		Description: "general",
		Mode:        config.AgentModeSubagent,
	}
	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	_, err = env.sessions.UpdatePermissionMode(t.Context(), parentSession.ID, session.PermissionModeAuto)
	require.NoError(t, err)
	coord := &coordinator{cfg: cfg, mailbox: mailbox.NewService(), sessions: env.sessions, agentRegistry: GlobalAgentRegistry()}

	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		require.Equal(t, config.AgentGeneral, requestedType)
		return &subagentMockSessionAgent{model: Model{CatwalkCfg: catwalk.Model{DefaultMaxTokens: 1000}, ModelCfg: config.SelectedModel{Provider: "test-provider", Model: "test-model"}}}, cfg.Config().Agents[config.AgentGeneral], nil
	}
	coord.subAgentScheduler = func(_ context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
		require.True(t, params.SkipHandoffReview)
		return withSubtaskToolResponseMetadata(fantasy.NewTextResponse("ok"), params.ToolCallID, "child-1", params.ParentMessageID, message.ToolResultSubtaskStatusCompleted), nil
	}

	resp, err := coord.runSubagents(t.Context(), subagentBatchParams{
		SessionID:      parentSession.ID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Tasks: []subagentTask{
			{Name: "a", Assignment: "task-a", SubagentType: config.AgentGeneral},
		},
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "- a: completed")
	require.Contains(t, resp.Content, message.SanitizedToolResultStub)
	require.NotContains(t, resp.Content, "ok")
}

func TestRunSubagents_KeepsFinishSummaryInAutoMode(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Agents[config.AgentGeneral] = config.Agent{
		ID:          config.AgentGeneral,
		Description: "general",
		Mode:        config.AgentModeSubagent,
	}
	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	_, err = env.sessions.UpdatePermissionMode(t.Context(), parentSession.ID, session.PermissionModeAuto)
	require.NoError(t, err)
	coord := &coordinator{cfg: cfg, mailbox: mailbox.NewService(), sessions: env.sessions, agentRegistry: GlobalAgentRegistry()}

	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		require.Equal(t, config.AgentGeneral, requestedType)
		return &subagentMockSessionAgent{model: Model{CatwalkCfg: catwalk.Model{DefaultMaxTokens: 1000}, ModelCfg: config.SelectedModel{Provider: "test-provider", Model: "test-model"}}}, cfg.Config().Agents[config.AgentGeneral], nil
	}
	coord.subAgentScheduler = func(_ context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
		require.True(t, params.SkipHandoffReview)
		response := withSubtaskToolResponseMetadata(
			fantasy.NewTextResponse("raw untrusted content"),
			params.ToolCallID,
			"child-1",
			params.ParentMessageID,
			message.ToolResultSubtaskStatusCompleted,
		)
		finish := message.ToolResultSubagentFinish{
			Status:  message.ToolResultSubtaskStatusCompleted,
			Summary: "found the code",
		}
		response = withSubagentFinishToolResponseMetadata(response, finish)
		return response, nil
	}

	resp, err := coord.runSubagents(t.Context(), subagentBatchParams{
		SessionID:      parentSession.ID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Tasks: []subagentTask{
			{Name: "a", Assignment: "task-a", SubagentType: config.AgentGeneral},
		},
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "- a: completed")
	require.Contains(t, resp.Content, "raw untrusted content")
	require.NotContains(t, resp.Content, message.SanitizedToolResultStub)
}

func TestRunSubagents_RetriesFailuresWithinBudget(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	retryBudget := 1
	cfg.Config().Agents[config.AgentGeneral] = config.Agent{
		ID:          config.AgentGeneral,
		Description: "general",
		Mode:        config.AgentModeSubagent,
		TaskGovernance: &config.TaskGovernance{
			RetryBudget: &retryBudget,
		},
	}
	coord := &coordinator{cfg: cfg, mailbox: mailbox.NewService(), sessions: env.sessions, agentRegistry: GlobalAgentRegistry()}

	var attempts int32
	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		return &subagentMockSessionAgent{model: Model{CatwalkCfg: catwalk.Model{DefaultMaxTokens: 1000}, ModelCfg: config.SelectedModel{Provider: "test-provider", Model: "test-model"}}}, cfg.Config().Agents[config.AgentGeneral], nil
	}
	coord.subAgentScheduler = func(_ context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
		attempt := atomic.AddInt32(&attempts, 1)
		if attempt == 1 {
			return withSubtaskToolResponseMetadata(fantasy.NewTextErrorResponse("try again"), params.ToolCallID, "", params.ParentMessageID, message.ToolResultSubtaskStatusFailed), nil
		}
		return withSubtaskToolResponseMetadata(fantasy.NewTextResponse("ok"), params.ToolCallID, params.ToolCallID, params.ParentMessageID, message.ToolResultSubtaskStatusCompleted), nil
	}

	resp, err := coord.runSubagents(t.Context(), subagentBatchParams{
		SessionID:      "session-1",
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Tasks: []subagentTask{
			{Name: "a", Assignment: "task-a", SubagentType: "general"},
		},
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, int32(2), atomic.LoadInt32(&attempts))
}

func TestRunSubagents_DoesNotRetryAfterSideEffects(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	retryBudget := 1
	cfg.Config().Agents[config.AgentGeneral] = config.Agent{
		ID:          config.AgentGeneral,
		Description: "general",
		Mode:        config.AgentModeSubagent,
		TaskGovernance: &config.TaskGovernance{
			RetryBudget: &retryBudget,
		},
	}
	coord := &coordinator{cfg: cfg, mailbox: mailbox.NewService(), sessions: env.sessions, agentRegistry: GlobalAgentRegistry()}

	var attempts int32
	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		return &subagentMockSessionAgent{model: Model{CatwalkCfg: catwalk.Model{DefaultMaxTokens: 1000}, ModelCfg: config.SelectedModel{Provider: "test-provider", Model: "test-model"}}}, cfg.Config().Agents[config.AgentGeneral], nil
	}
	coord.subAgentScheduler = func(_ context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
		atomic.AddInt32(&attempts, 1)
		resp := withSubtaskToolResponseMetadata(fantasy.NewTextErrorResponse("try again"), params.ToolCallID, "child-session-1", params.ParentMessageID, message.ToolResultSubtaskStatusFailed)
		resp = withSubagentFinishToolResponseMetadata(resp, message.ToolResultSubagentFinish{
			Status:       message.ToolResultSubtaskStatusFailed,
			Summary:      "try again",
			FilesTouched: []string{"internal/agent/coordinator.go"},
		})
		return resp, nil
	}

	resp, err := coord.runSubagents(t.Context(), subagentBatchParams{
		SessionID:      "session-1",
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Tasks:          []subagentTask{{Name: "a", Assignment: "task-a", SubagentType: "general"}},
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Equal(t, int32(1), atomic.LoadInt32(&attempts))
}

func TestRunSubagents_TimesOutTaskAttempts(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	timeoutSeconds := 1
	cfg.Config().Agents[config.AgentGeneral] = config.Agent{
		ID:          config.AgentGeneral,
		Description: "general",
		Mode:        config.AgentModeSubagent,
		TaskGovernance: &config.TaskGovernance{
			TimeoutSeconds: &timeoutSeconds,
		},
	}
	coord := &coordinator{cfg: cfg, mailbox: mailbox.NewService(), sessions: env.sessions, agentRegistry: GlobalAgentRegistry()}

	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		agent := &subagentMockSessionAgent{
			model: Model{CatwalkCfg: catwalk.Model{DefaultMaxTokens: 1000}, ModelCfg: config.SelectedModel{Provider: "test-provider", Model: "test-model"}},
			runFunc: func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		return agent, cfg.Config().Agents[config.AgentGeneral], nil
	}
	coord.subAgentScheduler = func(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
		_, err := params.Agent.Run(ctx, SessionAgentCall{Prompt: params.Prompt})
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return withSubtaskToolResponseMetadata(fantasy.NewTextResponse("ok"), params.ToolCallID, params.ToolCallID, params.ParentMessageID, message.ToolResultSubtaskStatusCompleted), nil
	}

	resp, err := coord.runSubagents(t.Context(), subagentBatchParams{
		SessionID:      "session-1",
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Tasks: []subagentTask{
			{Name: "a", Assignment: "task-a", SubagentType: "general"},
		},
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "a: canceled")
}

func TestRunSubagents_HonorsGraphTimeout(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	graphTimeout := 1
	cfg.Config().Agents[config.AgentGeneral] = config.Agent{
		ID:          config.AgentGeneral,
		Description: "general",
		Mode:        config.AgentModeSubagent,
		TaskGovernance: &config.TaskGovernance{
			GraphTimeoutSeconds: &graphTimeout,
		},
	}
	coord := &coordinator{cfg: cfg, mailbox: mailbox.NewService(), sessions: env.sessions, agentRegistry: GlobalAgentRegistry()}

	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		agent := &subagentMockSessionAgent{
			model: Model{CatwalkCfg: catwalk.Model{DefaultMaxTokens: 1000}, ModelCfg: config.SelectedModel{Provider: "test-provider", Model: "test-model"}},
			runFunc: func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		return agent, cfg.Config().Agents[config.AgentGeneral], nil
	}
	coord.subAgentScheduler = func(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
		_, err := params.Agent.Run(ctx, SessionAgentCall{Prompt: params.Prompt})
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return withSubtaskToolResponseMetadata(fantasy.NewTextResponse("ok"), params.ToolCallID, params.ToolCallID, params.ParentMessageID, message.ToolResultSubtaskStatusCompleted), nil
	}

	resp, err := coord.runSubagents(t.Context(), subagentBatchParams{
		SessionID:      "session-1",
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Tasks: []subagentTask{
			{Name: "a", Assignment: "task-a", SubagentType: "general"},
			{Name: "b", Assignment: "task-b", SubagentType: "general"},
		},
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "failed")
}

func TestRunSubagents_MailboxStopCancelsTask(t *testing.T) {
	env := testEnv(t)
	rootSession, err := env.sessions.Create(context.Background(), "subagent-stop")
	require.NoError(t, err)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	retryBudget := 1
	cfg.Config().Agents[config.AgentGeneral] = config.Agent{
		ID:          config.AgentGeneral,
		Description: "general",
		Mode:        config.AgentModeSubagent,
		TaskGovernance: &config.TaskGovernance{
			RetryBudget: &retryBudget,
		},
	}
	coord := &coordinator{cfg: cfg, mailbox: mailbox.NewService(), sessions: env.sessions, agentRegistry: GlobalAgentRegistry()}

	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		agent := &subagentMockSessionAgent{
			model: Model{CatwalkCfg: catwalk.Model{DefaultMaxTokens: 1000}, ModelCfg: config.SelectedModel{Provider: "test-provider", Model: "test-model"}},
			runFunc: func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
				time.Sleep(20 * time.Millisecond)
				return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "ok"}}}}, nil
			},
		}
		return agent, cfg.Config().Agents[config.AgentGeneral], nil
	}
	attempts := int32(0)
	coord.subAgentScheduler = func(_ context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
		attempt := atomic.AddInt32(&attempts, 1)
		if attempt == 1 {
			time.Sleep(30 * time.Millisecond)
			return withSubtaskToolResponseMetadata(
				fantasy.NewTextErrorResponse("retry"),
				params.ToolCallID,
				"",
				params.ParentMessageID,
				message.ToolResultSubtaskStatusFailed,
			), nil
		}
		return withSubtaskToolResponseMetadata(
			fantasy.NewTextResponse("ok"),
			params.ToolCallID,
			"child",
			params.ParentMessageID,
			message.ToolResultSubtaskStatusCompleted,
		), nil
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = coord.mailbox.Stop("call-stop", "a", "halted by parent")
	}()
	resp, err := coord.runSubagents(t.Context(), subagentBatchParams{
		SessionID:      rootSession.ID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-stop",
		Tasks: []subagentTask{
			{Name: "a", Assignment: "run", SubagentType: "general"},
		},
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "a: canceled")
	require.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(1))

	loadedSession, err := env.sessions.Get(context.Background(), rootSession.ID)
	require.NoError(t, err)
	require.Len(t, loadedSession.Todos, 1)
	require.Equal(t, session.TodoStatusCanceled, loadedSession.Todos[0].Status)
}

func TestRunSubagents_TruncatesTaskOutputsForModel(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	coord := &coordinator{cfg: cfg, mailbox: mailbox.NewService(), sessions: env.sessions, agentRegistry: GlobalAgentRegistry()}

	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		agent := &subagentMockSessionAgent{
			model: Model{CatwalkCfg: catwalk.Model{DefaultMaxTokens: 1000}, ModelCfg: config.SelectedModel{Provider: "test-provider", Model: "test-model"}},
		}
		return agent, config.Agent{ID: requestedType, Description: requestedType, Mode: config.AgentModeSubagent}, nil
	}
	coord.subAgentScheduler = func(_ context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
		big := strings.Repeat("x", subagentOutputPerTaskCharsLimit+200)
		return withSubtaskToolResponseMetadata(
			fantasy.NewTextResponse(big),
			params.ToolCallID,
			"child",
			params.ParentMessageID,
			message.ToolResultSubtaskStatusCompleted,
		), nil
	}

	resp, err := coord.runSubagents(t.Context(), subagentBatchParams{
		SessionID:      "session-1",
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Tasks: []subagentTask{
			{Name: "a", Assignment: "task-a", SubagentType: "general", Description: "alpha"},
		},
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Task outputs:")
	require.Contains(t, resp.Content, "[truncated;")
}

func TestCoordinatorCancelCancelsActiveSubAgents(t *testing.T) {
	t.Parallel()

	parent := &subagentMockSessionAgent{}
	subAgent := &subagentMockSessionAgent{}
	cancelAllCalled := make(chan struct{}, 1)
	subAgent.cancelAllFunc = func() {
		select {
		case cancelAllCalled <- struct{}{}:
		default:
		}
	}

	coord := &coordinator{currentAgent: parent}
	untrack := coord.trackActiveSubAgent("parent-session", subAgent)
	defer untrack()

	coord.Cancel("parent-session")

	select {
	case <-cancelAllCalled:
	case <-time.After(time.Second):
		t.Fatal("expected parent cancellation to cancel active subagent")
	}
}

func TestSubagentPromptWithMailboxMessagesAddsOmissionNotice(t *testing.T) {
	messages := make([]string, 0, subagentMailboxMessagesLimit+2)
	for i := 0; i < subagentMailboxMessagesLimit+2; i++ {
		messages = append(messages, fmt.Sprintf("message-%d", i))
	}
	prompt := promptWithMailboxMessages("run", messages)
	require.Contains(t, prompt, "earlier mailbox message(s) omitted")
}

func TestSubagentPromptWithMailboxMessagesKeepsUTF8WhenTrimmed(t *testing.T) {
	message := strings.Repeat("你", subagentMailboxPromptCharsLimit+20)
	prompt := promptWithMailboxMessages("run", []string{message})
	require.True(t, utf8.ValidString(prompt))
	require.Contains(t, prompt, "…")
}

func TestRunSubagents_RecoversFromTaskPanic(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	coord := &coordinator{cfg: cfg, mailbox: mailbox.NewService(), sessions: env.sessions, agentRegistry: GlobalAgentRegistry()}

	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		agent := &subagentMockSessionAgent{
			model: Model{CatwalkCfg: catwalk.Model{DefaultMaxTokens: 1000}, ModelCfg: config.SelectedModel{Provider: "test-provider", Model: "test-model"}},
			runFunc: func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
				return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "ok"}}}}, nil
			},
		}
		return agent, config.Agent{ID: requestedType, Description: requestedType, Mode: config.AgentModeSubagent}, nil
	}

	coord.subAgentScheduler = func(_ context.Context, _ subAgentParams) (fantasy.ToolResponse, error) {
		panic("scheduler crashed")
	}

	resp, err := coord.runSubagents(t.Context(), subagentBatchParams{
		SessionID:      "session-1",
		AgentMessageID: "msg-1",
		ToolCallID:     "call-panic",
		Tasks: []subagentTask{
			{Name: "a", Assignment: "task-a", SubagentType: "general"},
			{Name: "b", Assignment: "task-b", SubagentType: "general"},
		},
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "a: failed")
	require.Contains(t, resp.Content, "b: failed")
}
