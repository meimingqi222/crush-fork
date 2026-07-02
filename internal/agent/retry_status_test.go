package agent

import (
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestFormatTransientRetryMessageIncludesProviderReason(t *testing.T) {
	t.Parallel()

	msg := FormatTransientRetryMessage(
		&fantasy.ProviderError{StatusCode: 429, Message: "rate limit exceeded"},
		3*time.Second,
		2,
		5,
	)

	require.Contains(t, msg, "Service temporarily unavailable")
	require.Contains(t, msg, "Retrying in 3 seconds... (attempt 2/5)")
	require.Contains(t, msg, "Error: rate limit exceeded")
}

func TestFormatTransientRetryMessageWithoutReason(t *testing.T) {
	t.Parallel()

	msg := FormatTransientRetryMessage(nil, 5*time.Second, 1, 5)
	require.Equal(t, "Service temporarily unavailable. Retrying in 5 seconds... (attempt 1/5)", msg)
}

func TestParseRetryStatus(t *testing.T) {
	t.Parallel()

	reason, display, ok := ParseRetryStatus(
		"Service temporarily unavailable. Retrying in 40 seconds... (attempt 5/5)\nError: no available route for model",
	)
	require.True(t, ok)
	require.Equal(t, "no available route for model", reason)
	require.Contains(t, display, "Retrying in 40 seconds")
}

func TestParseRetryStatusLegacyMessage(t *testing.T) {
	t.Parallel()

	reason, display, ok := ParseRetryStatus(
		"Service temporarily unavailable. Retrying in 3 seconds... (attempt 1/5)",
	)
	require.True(t, ok)
	require.Empty(t, reason)
	require.Equal(t, "Service temporarily unavailable. Retrying in 3 seconds... (attempt 1/5)", display)
}

func TestRetryStatusCountdownOmitsProviderReason(t *testing.T) {
	t.Parallel()

	countdown, ok := RetryStatusCountdown(
		"Service temporarily unavailable. Retrying in 40 seconds... (attempt 5/5)\nError: no available route for model",
	)
	require.True(t, ok)
	require.Equal(t, "Service temporarily unavailable. Retrying in 40 seconds... (attempt 5/5)", countdown)
	require.NotContains(t, countdown, "Error:")
}

func TestRetryStatusDisplayTextAppendsProviderReason(t *testing.T) {
	t.Parallel()

	display, ok := RetryStatusDisplayText(
		"Service temporarily unavailable. Retrying in 40 seconds... (attempt 5/5)\nError: no available route for model",
	)
	require.True(t, ok)
	require.Equal(t, "Service temporarily unavailable. Retrying in 40 seconds... (attempt 5/5) — no available route for model", display)
}

func TestTransientErrorReasonUsesProviderMessage(t *testing.T) {
	t.Parallel()

	reason := TransientErrorReason(&fantasy.ProviderError{
		StatusCode: 503,
		Message:    "service unavailable",
	})
	require.Equal(t, "service unavailable", reason)
}
