package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripStreamingThinkTags(t *testing.T) {
	t.Parallel()

	require.Equal(t, "hello", stripStreamingThinkTags("hello"))
	require.Equal(t, "before  after", stripStreamingThinkTags("before <think>secret</think> after"))
	require.Equal(t, "before ", stripStreamingThinkTags("before <think>still open"))
	require.Equal(t, "", stripStreamingThinkTags("<think>only thinking</think>"))
}
