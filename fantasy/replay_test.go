package fantasy

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentStreamReplayRepreparesLogicalStep(t *testing.T) {
	t.Parallel()

	replayErr := errors.New("responses request requires full replay")
	streamCalls := 0
	prepareCalls := 0
	var replayFlags []bool

	model := &mockLanguageModel{
		streamFunc: func(context.Context, Call) (StreamResponse, error) {
			streamCalls++
			if streamCalls == 1 {
				return nil, replayErr
			}
			return func(yield func(StreamPart) bool) {
				yield(StreamPart{
					Type:         StreamPartTypeFinish,
					Usage:        Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
					FinishReason: FinishReasonStop,
				})
			}, nil
		},
	}

	agent := NewAgent(model)
	result, err := agent.Stream(context.Background(), AgentStreamCall{
		Prompt:     "hello",
		MaxRetries: ptr(0),
		ReplayRequired: func(err error) bool {
			return errors.Is(err, replayErr)
		},
		PrepareStep: func(ctx context.Context, options PrepareStepFunctionOptions) (context.Context, PrepareStepResult, error) {
			prepareCalls++
			replayFlags = append(replayFlags, options.Replay)
			return ctx, PrepareStepResult{}, nil
		},
	})

	require.NoError(t, err)
	require.Len(t, result.Steps, 1)
	require.Equal(t, 2, streamCalls)
	require.Equal(t, 2, prepareCalls)
	require.Equal(t, []bool{false, true}, replayFlags)
}

func ptr[T any](value T) *T {
	return &value
}
