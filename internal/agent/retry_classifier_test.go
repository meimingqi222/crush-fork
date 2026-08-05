package agent

import (
	"errors"
	"io"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestClassifyProviderError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want RetryErrorCategory
	}{
		{
			name: "auth error via AuthError flag",
			err:  &fantasy.ProviderError{AuthError: true, Title: "auth", Message: "invalid token"},
			want: RetryCategoryAuth,
		},
		{
			name: "auth error via 401 status",
			err:  &fantasy.ProviderError{StatusCode: 401, Title: "auth", Message: "unauthorized"},
			want: RetryCategoryAuth,
		},
		{
			name: "rate limit 429",
			err:  &fantasy.ProviderError{StatusCode: 429, Title: "rate limit", Message: "too many requests"},
			want: RetryCategoryRateLimit,
		},
		{
			name: "server error 500",
			err:  &fantasy.ProviderError{StatusCode: 500, Title: "server error", Message: "internal"},
			want: RetryCategoryServer,
		},
		{
			name: "server error 503",
			err:  &fantasy.ProviderError{StatusCode: 503, Title: "server error", Message: "unavailable"},
			want: RetryCategoryServer,
		},
		{
			name: "transport error (EOF)",
			err:  &fantasy.ProviderError{Title: "network error", Message: "unexpected EOF", Cause: io.ErrUnexpectedEOF},
			want: RetryCategoryTransport,
		},
		{
			name: "nil error",
			err:  nil,
			want: RetryCategoryUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyProviderError(tt.err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestClassifyProviderErrorContextOverflow(t *testing.T) {
	t.Parallel()

	err := errors.New("request too large: context length exceeded")
	require.Equal(t, RetryCategoryContext, ClassifyProviderError(err))
}

func TestClassifyProviderErrorUnexpectedEOF(t *testing.T) {
	t.Parallel()

	// After wrapRetryableNetworkErr wraps it, io.ErrUnexpectedEOF becomes
	// a retryable ProviderError.
	wrapped := wrapRetryableNetworkErr(io.ErrUnexpectedEOF)
	require.Equal(t, RetryCategoryTransport, ClassifyProviderError(wrapped))
}

func TestContainsAny(t *testing.T) {
	t.Parallel()

	require.True(t, containsAny("context length exceeded", "context length"))
	require.True(t, containsAny("maximum context reached", "maximum context"))
	require.False(t, containsAny("hello world", "foo"))
	require.False(t, containsAny("", "foo"))
}
