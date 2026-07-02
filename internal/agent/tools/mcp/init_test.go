package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMCPSession_CancelOnClose(t *testing.T) {
	defer goleak.VerifyNone(t)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server"}, nil)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()

	ctx, cancel := context.WithCancel(context.Background())

	client := mcp.NewClient(&mcp.Implementation{Name: "crush-test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)

	sess := &ClientSession{clientSession, cancel}

	// Verify the context is not cancelled before close.
	require.NoError(t, ctx.Err())

	err = sess.Close()
	require.NoError(t, err)

	// After Close, the context must be cancelled.
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

// TestUpdateState_StateCached_ClearsPromptsAndResources verifies that
// transitioning to StateCached clears any stale prompts and resources. The
// on-disk cache only stores tool definitions, so prompts and resources left
// over from a previous live connection must be removed to keep the maps
// consistent with Counts, which reports zero prompts and resources in the
// cached state.
func TestUpdateState_StateCached_ClearsPromptsAndResources(t *testing.T) {
	const name = "state-cached-clears-test"
	t.Cleanup(func() {
		states.Del(name)
		sessions.Del(name)
		allTools.Del(name)
		allPrompts.Del(name)
		allResources.Del(name)
	})

	// Seed prompts, resources, and tools as if a previous live connection
	// had populated them. Tools are preserved by the cache fallback, but
	// prompts and resources are not.
	allTools.Set(name, []*Tool{{Name: "cached-tool"}})
	allPrompts.Set(name, []*Prompt{{Name: "stale-prompt"}})
	allResources.Set(name, []*Resource{{URI: "stale-resource"}})

	updateState(name, StateCached, errors.New("connection lost"), nil, Counts{Tools: 1})

	info, ok := states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateCached, info.State)
	require.Equal(t, Counts{Tools: 1}, info.Counts)

	// Cached tools are retained so the LLM can still discover them.
	tools, hasTools := allTools.Get(name)
	require.True(t, hasTools)
	require.Len(t, tools, 1)

	// Prompts and resources must be cleared so Prompts()/Resources() do not
	// return stale data that contradicts the zero counts.
	_, hasPrompts := allPrompts.Get(name)
	require.False(t, hasPrompts, "stale prompts must be cleared in StateCached")
	_, hasResources := allResources.Get(name)
	require.False(t, hasResources, "stale resources must be cleared in StateCached")
}

// TestUpdateState_StateCircuitOpen_ClearsAllMaps verifies that transitioning
// to StateCircuitOpen clears all tools, prompts, and resources and zeroes
// the counts, consistent with StateError. The breaker only opens after the
// cache fallback has also failed, so no cached definitions should remain.
func TestUpdateState_StateCircuitOpen_ClearsAllMaps(t *testing.T) {
	const name = "state-circuit-open-clears-test"
	t.Cleanup(func() {
		states.Del(name)
		sessions.Del(name)
		allTools.Del(name)
		allPrompts.Del(name)
		allResources.Del(name)
	})

	// Seed tools, prompts, and resources as if a previous connection had
	// populated them.
	allTools.Set(name, []*Tool{{Name: "t1"}})
	allPrompts.Set(name, []*Prompt{{Name: "p1"}})
	allResources.Set(name, []*Resource{{URI: "r1"}})

	updateState(name, StateCircuitOpen, ErrCircuitOpen, nil, Counts{Tools: 1, Prompts: 1, Resources: 1})

	info, ok := states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateCircuitOpen, info.State)

	// Counts must be zeroed for consistency with StateError.
	require.Equal(t, Counts{}, info.Counts)

	// All maps must be cleared.
	_, hasTools := allTools.Get(name)
	require.False(t, hasTools, "tools must be cleared in StateCircuitOpen")
	_, hasPrompts := allPrompts.Get(name)
	require.False(t, hasPrompts, "prompts must be cleared in StateCircuitOpen")
	_, hasResources := allResources.Get(name)
	require.False(t, hasResources, "resources must be cleared in StateCircuitOpen")
}
