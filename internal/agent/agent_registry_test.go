package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentRegistry_RegisterAndGet(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}

	ref := r.Register(AgentRef{
		ID:          "test-1",
		DisplayName: "Test Agent",
		Kind:        AgentKindSub,
		Status:      AgentStatusRunning,
	})

	require.Equal(t, "test-1", ref.ID)
	require.Equal(t, "Test Agent", ref.DisplayName)

	got, ok := r.Get("test-1")
	require.True(t, ok)
	require.Equal(t, "Test Agent", got.DisplayName)

	_, ok = r.Get("nonexistent")
	require.False(t, ok)
}

func TestAgentRegistry_Unregister(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}

	r.Register(AgentRef{ID: "a", DisplayName: "A", Kind: AgentKindSub, Status: AgentStatusRunning})
	r.Register(AgentRef{ID: "b", DisplayName: "B", Kind: AgentKindSub, Status: AgentStatusRunning})

	r.Unregister("a")

	_, ok := r.Get("a")
	require.False(t, ok)

	_, ok = r.Get("b")
	require.True(t, ok)
}

func TestAgentRegistry_ListVisibleTo(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}

	r.Register(AgentRef{ID: "main", DisplayName: "Main", Kind: AgentKindMain, Status: AgentStatusRunning})
	r.Register(AgentRef{ID: "task-1", DisplayName: "Task 1", Kind: AgentKindSub, Status: AgentStatusRunning})
	r.Register(AgentRef{ID: "task-2", DisplayName: "Task 2", Kind: AgentKindSub, Status: AgentStatusIdle})
	r.Register(AgentRef{ID: "task-3", DisplayName: "Task 3", Kind: AgentKindSub, Status: AgentStatusCompleted})
	r.Register(AgentRef{ID: "task-4", DisplayName: "Task 4", Kind: AgentKindSub, Status: AgentStatusAborted})

	visible := r.ListVisibleTo("main")
	require.Len(t, visible, 2)

	ids := make(map[string]bool)
	for _, ref := range visible {
		ids[ref.ID] = true
	}
	require.True(t, ids["task-1"])
	require.True(t, ids["task-2"])
}

func TestAgentRegistry_SetStatus(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}

	r.Register(AgentRef{ID: "task-1", DisplayName: "Task 1", Kind: AgentKindSub, Status: AgentStatusRunning})

	r.SetStatus("task-1", AgentStatusCompleted)

	got, ok := r.Get("task-1")
	require.True(t, ok)
	require.Equal(t, AgentStatusCompleted, got.Status)
}

func TestAgentRegistry_AsIrcRegistry(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}

	r.Register(AgentRef{ID: "0-Main", DisplayName: "Main", Kind: AgentKindMain, Status: AgentStatusRunning})
	r.Register(AgentRef{ID: "0-Main::t1", DisplayName: "Explore", Kind: AgentKindSub, Status: AgentStatusRunning, ParentID: "0-Main"})
	r.Register(AgentRef{ID: "0-Main::t2", DisplayName: "Fix", Kind: AgentKindSub, Status: AgentStatusCompleted, ParentID: "0-Main"})

	irc := r.AsIrcRegistry()

	peer, ok := irc.Get("0-Main::t1")
	require.True(t, ok)
	require.Equal(t, "0-Main::t1", peer.ID)
	require.Equal(t, "Explore", peer.DisplayName)
	require.Equal(t, "sub", peer.Kind)
	require.Equal(t, "running", peer.Status)
	require.Equal(t, "0-Main", peer.ParentID)

	_, ok = irc.Get("0-Main::t2")
	require.True(t, ok)

	_, ok = irc.Get("nonexistent")
	require.False(t, ok)

	visible := irc.ListVisibleTo("0-Main")
	require.Len(t, visible, 1)
	require.Equal(t, "0-Main::t1", visible[0].ID)
}

func TestAgentRegistry_OnChange(t *testing.T) {
	t.Parallel()

	t.Run("fires on register and set status", func(t *testing.T) {
		t.Parallel()
		r := &AgentRegistry{refs: make(map[string]*AgentRef)}

		var changes int32
		r.OnChange(func() { changes++ })

		r.Register(AgentRef{ID: "a", DisplayName: "A", Kind: AgentKindSub, Status: AgentStatusRunning})
		r.SetStatus("a", AgentStatusCompleted)

		require.GreaterOrEqual(t, changes, int32(2))
	})

	t.Run("unsubscribe stops notifications", func(t *testing.T) {
		t.Parallel()
		r := &AgentRegistry{refs: make(map[string]*AgentRef)}

		var changes int32
		unsub := r.OnChange(func() { changes++ })

		r.Register(AgentRef{ID: "a", DisplayName: "A", Kind: AgentKindSub, Status: AgentStatusRunning})
		beforeUnsub := changes

		unsub()

		r.Register(AgentRef{ID: "b", DisplayName: "B", Kind: AgentKindSub, Status: AgentStatusRunning})
		afterSecond := changes

		require.Equal(t, beforeUnsub, afterSecond)
	})
}

func TestAgentRegistry_Reset(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}

	r.Register(AgentRef{ID: "a", DisplayName: "A", Kind: AgentKindSub, Status: AgentStatusRunning})
	r.Reset()

	_, ok := r.Get("a")
	require.False(t, ok)
}

func TestRenderIrcPeerRoster(t *testing.T) {
	t.Parallel()

	t.Run("returns empty when no peers visible", func(t *testing.T) {
		t.Parallel()
		r := &AgentRegistry{refs: make(map[string]*AgentRef)}
		r.Register(AgentRef{ID: "only", DisplayName: "Only", Kind: AgentKindMain, Status: AgentStatusRunning})
		result := renderIrcPeerRoster(r, "only")
		require.Empty(t, result)
	})

	t.Run("renders visible peers", func(t *testing.T) {
		t.Parallel()
		r := &AgentRegistry{refs: make(map[string]*AgentRef)}
		r.Register(AgentRef{ID: "main", DisplayName: "Main", Kind: AgentKindMain, Status: AgentStatusRunning})
		r.Register(AgentRef{ID: "sub-1", DisplayName: "Explore", Kind: AgentKindSub, Status: AgentStatusRunning, ParentID: "main"})
		r.Register(AgentRef{ID: "sub-2", DisplayName: "Fix", Kind: AgentKindSub, Status: AgentStatusIdle, ParentID: "main"})

		result := renderIrcPeerRoster(r, "sub-1")
		require.Contains(t, result, "<irc_peers>")
		require.Contains(t, result, "</irc_peers>")
		require.Contains(t, result, "`main`")
		require.Contains(t, result, "Main")
		require.Contains(t, result, "`sub-2`")
		require.Contains(t, result, "Fix")
		require.NotContains(t, result, "sub-1")
	})

	t.Run("excludes completed and aborted peers", func(t *testing.T) {
		t.Parallel()
		r := &AgentRegistry{refs: make(map[string]*AgentRef)}
		r.Register(AgentRef{ID: "main", DisplayName: "Main", Kind: AgentKindMain, Status: AgentStatusRunning})
		r.Register(AgentRef{ID: "done", DisplayName: "Done", Kind: AgentKindSub, Status: AgentStatusCompleted})
		r.Register(AgentRef{ID: "dead", DisplayName: "Dead", Kind: AgentKindSub, Status: AgentStatusAborted})

		result := renderIrcPeerRoster(r, "main")
		require.NotContains(t, result, "Done")
		require.NotContains(t, result, "Dead")
	})
}
