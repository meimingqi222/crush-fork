package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestDetectCacheInvalidation(t *testing.T) {
	t.Parallel()

	prev := message.Usage{CacheReadTokens: 10_000, InputTokens: 100, OutputTokens: 50}
	current := message.Usage{CacheReadTokens: 0, CacheWriteTokens: 8_000, InputTokens: 2_000, OutputTokens: 40}
	n, ok := DetectCacheInvalidation(prev, current)
	require.True(t, ok)
	require.Equal(t, int64(10_000), n)

	_, ok = DetectCacheInvalidation(message.Usage{CacheReadTokens: 100}, current)
	require.False(t, ok)

	_, ok = DetectCacheInvalidation(prev, message.Usage{CacheReadTokens: 500, CacheWriteTokens: 0, InputTokens: 5000})
	require.False(t, ok)
}
