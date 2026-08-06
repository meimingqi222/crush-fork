package agent

import (
	"context"
	"errors"
	"io"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

type stubLanguageModel struct {
	stream   func(context.Context, fantasy.Call) (fantasy.StreamResponse, error)
	generate func(context.Context, fantasy.Call) (*fantasy.Response, error)
}

func (m stubLanguageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	if m.generate != nil {
		return m.generate(ctx, call)
	}
	panic("unexpected Generate call")
}

func (m stubLanguageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	return m.stream(ctx, call)
}

func (m stubLanguageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	panic("unexpected GenerateObject call")
}

func (m stubLanguageModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	panic("unexpected StreamObject call")
}

func (m stubLanguageModel) Provider() string {
	return "test"
}

func (m stubLanguageModel) Model() string {
	return "test"
}

func TestRetryableStreamModelWrapsUnexpectedEOFBeforeToolCall(t *testing.T) {
	t.Parallel()

	model := retryableStreamModel{stubLanguageModel{
		stream: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				yield(fantasy.StreamPart{
					Type:  fantasy.StreamPartTypeError,
					Error: io.ErrUnexpectedEOF,
				})
			}, nil
		},
	}}

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)

	var gotErr error
	stream(func(part fantasy.StreamPart) bool {
		gotErr = part.Error
		return true
	})

	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, gotErr, &providerErr)
	require.ErrorIs(t, providerErr.Cause, io.ErrUnexpectedEOF)
}

func TestRetryableStreamModelDoesNotWrapUnexpectedEOFAfterToolCall(t *testing.T) {
	t.Parallel()

	model := retryableStreamModel{stubLanguageModel{
		stream: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				if !yield(fantasy.StreamPart{
					Type:         fantasy.StreamPartTypeToolCall,
					ID:           "tool-1",
					ToolCallName: "bash",
				}) {
					return
				}
				yield(fantasy.StreamPart{
					Type:  fantasy.StreamPartTypeError,
					Error: io.ErrUnexpectedEOF,
				})
			}, nil
		},
	}}

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)

	var gotErr error
	stream(func(part fantasy.StreamPart) bool {
		if part.Type == fantasy.StreamPartTypeError {
			gotErr = part.Error
		}
		return true
	})

	require.ErrorIs(t, gotErr, io.ErrUnexpectedEOF)
	var providerErr *fantasy.ProviderError
	require.False(t, errors.As(gotErr, &providerErr), "tool-call failures must not become retryable provider errors")
}

func TestRetryableStreamModelDoesNotWrapUnexpectedEOFAfterToolInputStart(t *testing.T) {
	t.Parallel()

	model := retryableStreamModel{stubLanguageModel{
		stream: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				if !yield(fantasy.StreamPart{
					Type:         fantasy.StreamPartTypeToolInputStart,
					ID:           "tool-1",
					ToolCallName: "ls",
				}) {
					return
				}
				yield(fantasy.StreamPart{
					Type:  fantasy.StreamPartTypeError,
					Error: io.ErrUnexpectedEOF,
				})
			}, nil
		},
	}}

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)

	var gotErr error
	stream(func(part fantasy.StreamPart) bool {
		if part.Type == fantasy.StreamPartTypeError {
			gotErr = part.Error
		}
		return true
	})

	require.ErrorIs(t, gotErr, io.ErrUnexpectedEOF)
	var providerErr *fantasy.ProviderError
	require.False(t, errors.As(gotErr, &providerErr), "tool-input failures must not become retryable provider errors")
}

func TestWithRetryFailureDetails(t *testing.T) {
	t.Parallel()

	t.Run("returns details unchanged when no retries happened", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "stream idle timeout: no data received for 45s", withRetryFailureDetails("stream idle timeout: no data received for 45s", 0))
	})

	t.Run("prefixes details when retries were exhausted", func(t *testing.T) {
		t.Parallel()
		require.Equal(
			t,
			"Retried 2 times, but the request still failed. stream idle timeout: no data received for 45s",
			withRetryFailureDetails("stream idle timeout: no data received for 45s", 2),
		)
	})

	t.Run("handles singular retry wording", func(t *testing.T) {
		t.Parallel()
		require.Equal(
			t,
			"Retried 1 time, but the request still failed.",
			withRetryFailureDetails("", 1),
		)
	})
}

func TestProviderErrorTitle(t *testing.T) {
	t.Parallel()

	t.Run("returns No available route for aggregator exhaustion messages", func(t *testing.T) {
		t.Parallel()
		for _, msg := range []string{
			"No available route for model",
			"All accounts exhausted",
			"no available model: claude-sonnet",
			"no upstream available",
		} {
			providerErr := &fantasy.ProviderError{Title: "too many requests", Message: msg}
			require.Equal(t, "No available route", providerErrorTitle(providerErr), "message: %q", msg)
		}
	})

	t.Run("capitalizes HTTP status text for genuine rate limits", func(t *testing.T) {
		t.Parallel()
		providerErr := &fantasy.ProviderError{Title: "too many requests", Message: "rate limit exceeded"}
		require.Equal(t, "Too Many Requests", providerErrorTitle(providerErr))
	})

	t.Run("returns empty when title is empty", func(t *testing.T) {
		t.Parallel()
		providerErr := &fantasy.ProviderError{Message: "no available route for model"}
		require.Equal(t, "", providerErrorTitle(providerErr))
	})

	t.Run("returns empty for nil error", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "", providerErrorTitle(nil))
	})
}
