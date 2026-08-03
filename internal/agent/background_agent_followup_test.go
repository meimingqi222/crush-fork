package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// waitForCond polls until cond holds, so these tests do not depend on the
// command loop's scheduling.
func waitForCond(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// TestRegisterWithoutRunnerRejectsFollowUps documents why runBackgroundTask
// must use RegisterNamed with a real runner. Register() attaches no runner, so
// the entry has no command channel and every send_message(agent_id) follow-up
// fails -- which is exactly how the feature shipped broken: the tool advertised
// follow-ups while the only spawn path produced agents that could not take one.
func TestRegisterWithoutRunnerRejectsFollowUps(t *testing.T) {
	t.Parallel()

	r := newBackgroundAgentRegistry()
	id := r.Register("no runner")

	_, err := r.Enqueue(id, backgroundAgentCommand{Prompt: "continue"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not accept follow-up prompts")
}

// TestFollowUpContinuesSameChildSession is the core contract: a follow-up must
// reuse the child session created by the first run. runSubAgentDirect keys off
// ExistingSessionID to skip session creation *and* the handoff prefix, so
// reusing the id is what preserves the subagent's history instead of re-seeding
// it with a summary.
func TestFollowUpContinuesSameChildSession(t *testing.T) {
	t.Parallel()

	r := newBackgroundAgentRegistry()

	var (
		mu sync.Mutex
		// sessionAtEntry records what the runner saw before each command, which
		// is what runBackgroundTask feeds to ExistingSessionID.
		sessionAtEntry []string
		prompts        []string
		child          string
	)
	runner := func(_ context.Context, cmd backgroundAgentCommand) backgroundAgentRunResult {
		mu.Lock()
		prompts = append(prompts, cmd.Prompt)
		sessionAtEntry = append(sessionAtEntry, child)
		if child == "" {
			child = "child-session-1"
		}
		out := child
		mu.Unlock()
		return backgroundAgentRunResult{
			ChildSessionID: out,
			Status:         backgroundAgentStatusCompleted,
			Content:        "done",
		}
	}

	id := r.RegisterNamed("", "general", "probe", runner)

	_, err := r.Enqueue(id, backgroundAgentCommand{Prompt: "first"})
	require.NoError(t, err, "initial command must be accepted")
	waitForCond(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(prompts) == 1
	}, "first command")

	// The agent has completed; a follow-up must still be accepted.
	_, err = r.Enqueue(id, backgroundAgentCommand{Prompt: "follow-up"})
	require.NoError(t, err, "follow-up after completion must be accepted")
	waitForCond(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(prompts) == 2
	}, "follow-up command")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"first", "follow-up"}, prompts)
	require.Empty(t, sessionAtEntry[0], "first run creates the child session")
	require.Equal(t, "child-session-1", sessionAtEntry[1],
		"follow-up must continue the existing child session")

	entry, ok := r.Get(id)
	require.True(t, ok)
	require.Equal(t, backgroundAgentStatusCompleted, entry.Status)
	require.Equal(t, "child-session-1", entry.ChildSessionID)
}

// TestFollowUpTasksPreserveSpecialization checks the follow-up keeps running as
// the same specialist in the same workspace; only the assignment changes.
func TestFollowUpTasksPreserveSpecialization(t *testing.T) {
	t.Parallel()

	original := []subagentTask{{
		Name:         "explorer",
		Description:  "map the auth flow",
		Assignment:   "find every call site",
		SubagentType: "explore",
		Role:         "researcher",
		Isolation:    "worktree",
	}}

	got := followUpTasks(original, "now check the error paths")
	require.Len(t, got, 1)
	require.Equal(t, "now check the error paths", got[0].Assignment)
	require.Equal(t, "explore", got[0].SubagentType)
	require.Equal(t, "researcher", got[0].Role)
	require.Equal(t, "worktree", got[0].Isolation)
	require.Equal(t, "explorer", got[0].Name)

	// A fan-out has no single session to continue, so it is left untouched.
	batch := []subagentTask{{Name: "a"}, {Name: "b"}}
	require.Equal(t, batch, followUpTasks(batch, "ignored"))
}

// TestMergeCancelPropagatesEitherSide pins the context contract: values come
// from the spawning turn (which has already returned), cancellation from the
// per-command context the command loop owns.
func TestMergeCancelPropagatesEitherSide(t *testing.T) {
	t.Parallel()

	type ctxKey struct{}
	base := context.WithValue(context.Background(), ctxKey{}, "kept")

	t.Run("trigger cancels merged", func(t *testing.T) {
		t.Parallel()
		trigger, cancelTrigger := context.WithCancel(context.Background())
		merged, cancel := mergeCancel(base, trigger)
		defer cancel()

		require.Equal(t, "kept", merged.Value(ctxKey{}), "values must survive")
		cancelTrigger()
		select {
		case <-merged.Done():
		case <-time.After(time.Second):
			t.Fatal("merged context did not follow trigger cancellation")
		}
	})

	t.Run("cancel stops the watcher", func(t *testing.T) {
		t.Parallel()
		trigger := context.Background() // never cancelled
		merged, cancel := mergeCancel(base, trigger)
		cancel()
		select {
		case <-merged.Done():
		case <-time.After(time.Second):
			t.Fatal("merged context did not honor its own cancel")
		}
	})
}
