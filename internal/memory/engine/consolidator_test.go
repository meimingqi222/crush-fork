package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mockConsolidatorDeps holds mock callbacks used across tests.
type mockConsolidatorDeps struct {
	getExistingFn func(ctx context.Context) ([]MemoryEvent, error)
	analyzeFn     func(ctx context.Context, episodes, existing string) ([]ConsolidatedEvent, error)
	clock         func() time.Time
}

func newMockConsolidator(deps mockConsolidatorDeps) *LLMConsolidator {
	return &LLMConsolidator{
		getExisting:   deps.getExistingFn,
		analyzeEvents: deps.analyzeFn,
		clock:         deps.clock,
	}
}

func defaultExistingFn(events []MemoryEvent) func(ctx context.Context) ([]MemoryEvent, error) {
	return func(_ context.Context) ([]MemoryEvent, error) {
		return events, nil
	}
}

func defaultConsolidateFn(t *testing.T, consolidated []ConsolidatedEvent) func(ctx context.Context, episodes, existing string) ([]ConsolidatedEvent, error) {
	t.Helper()
	return func(_ context.Context, _, _ string) ([]ConsolidatedEvent, error) {
		return consolidated, nil
	}
}

func TestConsolidator_EmptyInput(t *testing.T) {
	t.Parallel()
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: func(_ context.Context, _, _ string) ([]ConsolidatedEvent, error) {
			return []ConsolidatedEvent{{Kind: MemoryKindDecision}}, nil
		},
		clock: fixedClock,
	})

	events, err := con.Consolidate(context.Background(), nil)
	require.NoError(t, err)
	require.Nil(t, events, "nil input should return nil")

	events, err = con.Consolidate(context.Background(), []MemoryEvent{})
	require.NoError(t, err)
	require.Nil(t, events, "empty input should return nil")
}

func TestConsolidator_EmptyOutput(t *testing.T) {
	t.Parallel()
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn:     defaultConsolidateFn(t, nil),
		clock:         fixedClock,
	})

	input := []MemoryEvent{testEvent(MemoryScopeSession, MemoryKindDecision, "some decision")}
	events, err := con.Consolidate(context.Background(), input)
	require.NoError(t, err)
	require.Nil(t, events, "empty analysis should return nil")
}

func TestConsolidator_GetExistingError(t *testing.T) {
	t.Parallel()
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: func(_ context.Context) ([]MemoryEvent, error) {
			return nil, errors.New("existing fetch failed")
		},
	})
	input := []MemoryEvent{testEvent(MemoryScopeSession, MemoryKindDecision, "some decision")}
	_, err := con.Consolidate(context.Background(), input)
	require.Error(t, err)
	require.Contains(t, err.Error(), "getting existing consolidated events")
}

func TestConsolidator_AnalysisError(t *testing.T) {
	t.Parallel()
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: func(_ context.Context, _, _ string) ([]ConsolidatedEvent, error) {
			return nil, errors.New("LLM call failed")
		},
		clock: fixedClock,
	})
	input := []MemoryEvent{testEvent(MemoryScopeSession, MemoryKindDecision, "some decision")}
	_, err := con.Consolidate(context.Background(), input)
	require.Error(t, err)
	require.Contains(t, err.Error(), "consolidation analysis failed")
}

func TestConsolidator_SingleConsolidation(t *testing.T) {
	t.Parallel()
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: defaultConsolidateFn(t, []ConsolidatedEvent{
			{
				Kind:       MemoryKindDecision,
				Scope:      MemoryScopeProject,
				Content:    "Use SQLite for event storage",
				Summary:    "Storage decision",
				Confidence: 0.9,
				Importance: 0.8,
				Tags:       []string{"database", "sqlite"},
			},
		}),
		clock: fixedClock,
	})

	input := []MemoryEvent{
		testEvent(MemoryScopeSession, MemoryKindDecision, "use sqlite"),
	}
	events, err := con.Consolidate(context.Background(), input)
	require.NoError(t, err)
	require.Len(t, events, 1)

	evt := events[0]
	require.Equal(t, MemoryKindDecision, evt.Kind)
	require.Equal(t, MemoryScopeProject, evt.Scope)
	require.Equal(t, "Use SQLite for event storage", evt.Content)
	require.Equal(t, "Storage decision", evt.Summary)
	require.Equal(t, 0.9, evt.Confidence)
	require.Equal(t, 0.8, evt.Importance)
	require.Equal(t, []string{"database", "sqlite"}, evt.Tags)
	require.Equal(t, fixedClock(), evt.CreatedAt)
	require.Equal(t, fixedClock(), evt.UpdatedAt)
	require.Contains(t, evt.ID, "con-decision-")
	require.Nil(t, evt.Supersedes)
}

func TestConsolidator_MultiConsolidation(t *testing.T) {
	t.Parallel()
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: defaultConsolidateFn(t, []ConsolidatedEvent{
			{
				Kind:       MemoryKindDecision,
				Scope:      MemoryScopeProject,
				Content:    "Use SQLite for event storage",
				Summary:    "Storage decision",
				Confidence: 0.9,
				Importance: 0.8,
			},
			{
				Kind:       MemoryKindPreference,
				Scope:      MemoryScopeUser,
				Content:    "User prefers concise code",
				Summary:    "Code style",
				Confidence: 0.8,
				Importance: 0.5,
				Tags:       []string{"style"},
			},
			{
				Kind:       MemoryKindPitfall,
				Scope:      MemoryScopeProject,
				Content:    "Avoid global state in handlers",
				Summary:    "Gotcha",
				Confidence: 0.7,
				Importance: 0.9,
			},
		}),
		clock: fixedClock,
	})

	input := []MemoryEvent{
		testEvent(MemoryScopeSession, MemoryKindDecision, "use sqlite"),
		testEvent(MemoryScopeSession, MemoryKindPreference, "concise code"),
		testEvent(MemoryScopeSession, MemoryKindPitfall, "global state bad"),
	}
	events, err := con.Consolidate(context.Background(), input)
	require.NoError(t, err)
	require.Len(t, events, 3)

	require.Equal(t, MemoryKindDecision, events[0].Kind)
	require.Equal(t, MemoryKindPreference, events[1].Kind)
	require.Equal(t, MemoryKindPitfall, events[2].Kind)

	// Each event gets a unique ID
	ids := make(map[string]bool)
	for _, e := range events {
		ids[e.ID] = true
	}
	require.Len(t, ids, 3, "each event must have a unique ID")
}

func TestConsolidator_Supersedes(t *testing.T) {
	t.Parallel()
	existingID := "existing-decision-1"
	existing := []MemoryEvent{{
		ID:      existingID,
		Kind:    MemoryKindDecision,
		Scope:   MemoryScopeProject,
		Content: "Old decision: use MySQL",
	}}

	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(existing),
		analyzeFn: func(_ context.Context, episodes, existingText string) ([]ConsolidatedEvent, error) {
			// Verify the existing events were passed to the LLM
			require.Contains(t, existingText, existingID)
			require.Contains(t, existingText, "use MySQL")
			return []ConsolidatedEvent{
				{
					Kind:       MemoryKindDecision,
					Scope:      MemoryScopeProject,
					Content:    "Use SQLite for event storage",
					Summary:    "Storage decision updated",
					Confidence: 0.95,
					Importance: 0.9,
					Supersedes: &existingID,
				},
			}, nil
		},
		clock: fixedClock,
	})

	input := []MemoryEvent{
		testEvent(MemoryScopeSession, MemoryKindDecision, "use sqlite instead of mysql"),
	}
	events, err := con.Consolidate(context.Background(), input)
	require.NoError(t, err)
	require.Len(t, events, 1)

	require.NotNil(t, events[0].Supersedes)
	require.Equal(t, existingID, *events[0].Supersedes)
}

func TestConsolidator_DefaultValues(t *testing.T) {
	t.Parallel()
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: defaultConsolidateFn(t, []ConsolidatedEvent{
			{
				Kind:    MemoryKindDecision,
				Content: "Minimal event with defaults",
			},
		}),
		clock: fixedClock,
	})

	input := []MemoryEvent{
		testEvent(MemoryScopeSession, MemoryKindDecision, "minimal decision"),
	}
	events, err := con.Consolidate(context.Background(), input)
	require.NoError(t, err)
	require.Len(t, events, 1)

	// Zero confidence/importance should get defaults
	require.Equal(t, 0.7, events[0].Confidence)
	require.Equal(t, 0.5, events[0].Importance)
	// Empty scope should default to project
	require.Equal(t, MemoryScopeProject, events[0].Scope)
}

func TestConsolidator_EventsFormatted(t *testing.T) {
	t.Parallel()
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: func(_ context.Context, episodes, _ string) ([]ConsolidatedEvent, error) {
			require.Contains(t, episodes, "Session: sess-format-test")
			require.Contains(t, episodes, "Kind: decision")
			require.Contains(t, episodes, "Scope: session")
			require.Contains(t, episodes, "Content: use sqlite for storage")
			require.Contains(t, episodes, "Tags: database, sqlite")
			return nil, nil
		},
		clock: fixedClock,
	})

	evt := MemoryEvent{
		ID:      "evt-format",
		Scope:   MemoryScopeSession,
		Kind:    MemoryKindDecision,
		Content: "use sqlite for storage",
		Source: MemorySourceRef{
			SessionID: "sess-format-test",
		},
		Confidence: 0.8,
		Importance: 0.6,
		Tags:       []string{"database", "sqlite"},
	}
	input := []MemoryEvent{evt}
	events, err := con.Consolidate(context.Background(), input)
	require.NoError(t, err)
	require.Nil(t, events)
}

func TestConsolidator_EventKinds(t *testing.T) {
	t.Parallel()
	allKinds := []struct {
		kind  MemoryKind
		scope MemoryScope
	}{
		{MemoryKindDecision, MemoryScopeProject},
		{MemoryKindPreference, MemoryScopeUser},
		{MemoryKindProcedure, MemoryScopeProject},
		{MemoryKindPitfall, MemoryScopeProject},
		{MemoryKindReference, MemoryScopeProject},
		{MemoryKindTaskState, MemoryScopeSession},
	}

	consolidated := make([]ConsolidatedEvent, len(allKinds))
	for i, k := range allKinds {
		consolidated[i] = ConsolidatedEvent{
			Kind:       k.kind,
			Scope:      k.scope,
			Content:    string(k.kind) + " consolidated",
			Confidence: 0.8,
			Importance: 0.6,
		}
	}

	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn:     defaultConsolidateFn(t, consolidated),
		clock:         fixedClock,
	})

	input := make([]MemoryEvent, len(allKinds))
	for i, k := range allKinds {
		input[i] = testEvent(MemoryScopeSession, k.kind, "source event for "+string(k.kind))
	}

	result, err := con.Consolidate(context.Background(), input)
	require.NoError(t, err)
	require.Len(t, result, len(allKinds))
	for i, k := range allKinds {
		require.Equal(t, k.kind, result[i].Kind, "index %d kind mismatch", i)
		require.Equal(t, k.scope, result[i].Scope, "index %d scope mismatch", i)
	}
}

func TestLLMConsolidator_ImplementsConsolidator(t *testing.T) {
	t.Parallel()
	var _ Consolidator = (*LLMConsolidator)(nil)
}

func TestEngine_TriggerConsolidation(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})
	ctx := context.Background()

	// Pre-populate some episodic events
	evt1 := testEvent(MemoryScopeSession, MemoryKindDecision, "Use SQLite for storage")
	evt2 := testEvent(MemoryScopeSession, MemoryKindPreference, "Prefer Go modules")
	err := eng.store.Append(ctx, evt1)
	require.NoError(t, err)
	err = eng.store.Append(ctx, evt2)
	require.NoError(t, err)

	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: defaultConsolidateFn(t, []ConsolidatedEvent{
			{
				Kind:       MemoryKindDecision,
				Scope:      MemoryScopeProject,
				Content:    "Use SQLite for event storage",
				Summary:    "Storage decision",
				Confidence: 0.9,
				Importance: 0.8,
				Tags:       []string{"database", "sqlite"},
			},
		}),
		clock: fixedClock,
	})
	eng.SetConsolidator(con)

	err = eng.TriggerConsolidation(ctx)
	require.NoError(t, err)

	// Verify consolidated event was stored
	scope := MemoryScopeProject
	events, err := eng.store.Query(ctx, EventFilter{Scope: &scope})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "Use SQLite for event storage", events[0].Content)
	require.Equal(t, MemoryKindDecision, events[0].Kind)
	require.NotNil(t, eng.lastConsolidationRun)
	require.True(t, eng.lastConsolidatedWatermark > 0)
	require.Equal(t, int64(2), eng.lastConsolidatedWatermark, "should reflect highest processed event watermark")
}

func TestEngine_TriggerConsolidationNoConsolidator(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})

	err := eng.TriggerConsolidation(context.Background())
	require.NoError(t, err, "should not error when no consolidator is set")
}

func TestEngine_TriggerConsolidationNoEvents(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: func(_ context.Context, _, _ string) ([]ConsolidatedEvent, error) {
			t.Error("should not be called with no events")
			return nil, nil
		},
	})
	eng.SetConsolidator(con)

	err := eng.TriggerConsolidation(context.Background())
	require.NoError(t, err)
}

func TestEngine_TriggerConsolidationWatermarkAdvance(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})
	ctx := context.Background()

	// Add events with increasing watermarks
	for i := 0; i < 3; i++ {
		evt := testEvent(MemoryScopeSession, MemoryKindDecision, "event "+fmt.Sprintf("%d", i))
		err := eng.store.Append(ctx, evt)
		require.NoError(t, err)
	}

	callCount := 0
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: func(_ context.Context, _, _ string) ([]ConsolidatedEvent, error) {
			callCount++
			return []ConsolidatedEvent{
				{Kind: MemoryKindDecision, Scope: MemoryScopeProject, Content: "consolidated", Confidence: 0.8, Importance: 0.6},
			}, nil
		},
		clock: fixedClock,
	})
	eng.SetConsolidator(con)

	// First call: processes events with watermarks 1,2,3
	err := eng.TriggerConsolidation(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, callCount)
	require.Equal(t, int64(3), eng.lastConsolidatedWatermark)

	// Second call: should process nothing (watermark already at 3)
	err = eng.TriggerConsolidation(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, callCount, "should not call consolidator again with no new events")
}

func TestEngine_TriggerConsolidationPersistsWatermark(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()

	eng := New(db, Config{Enabled: true})
	require.NoError(t, eng.store.Append(ctx, testEvent(MemoryScopeSession, MemoryKindDecision, "persist watermark")))

	callCount := 0
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: func(_ context.Context, _, _ string) ([]ConsolidatedEvent, error) {
			callCount++
			return []ConsolidatedEvent{
				{Kind: MemoryKindDecision, Scope: MemoryScopeProject, Content: "persisted consolidation checkpoint", Confidence: 0.8, Importance: 0.6},
			}, nil
		},
		clock: fixedClock,
	})
	eng.SetConsolidator(con)

	require.NoError(t, eng.TriggerConsolidation(ctx))
	require.Equal(t, 1, callCount)

	restarted := New(db, Config{Enabled: true})
	restarted.SetConsolidator(con)
	require.NoError(t, restarted.TriggerConsolidation(ctx))
	require.Equal(t, 1, callCount, "new engine instances should not reprocess consolidated events")
}

func TestEngine_TriggerConsolidationSkipsWorkingMemory(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})
	ctx := context.Background()

	evt := testEvent(MemoryScopeSession, MemoryKindWorkingMemory, "transient session state")
	require.NoError(t, eng.store.Append(ctx, evt))

	callCount := 0
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: func(_ context.Context, _, _ string) ([]ConsolidatedEvent, error) {
			callCount++
			return nil, nil
		},
		clock: fixedClock,
	})
	eng.SetConsolidator(con)

	require.NoError(t, eng.TriggerConsolidation(ctx))
	require.Equal(t, 0, callCount, "working memory should not be promoted into long-term consolidation")
	require.Equal(t, int64(1), eng.lastConsolidatedWatermark)
}

func TestEngine_TriggerConsolidationDoesNotSkipDurableAfterTransientBatch(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})
	ctx := context.Background()

	for i := 0; i < 500; i++ {
		require.NoError(t, eng.store.Append(ctx, testEvent(
			MemoryScopeSession,
			MemoryKindWorkingMemory,
			fmt.Sprintf("transient state %d", i),
		)))
	}
	require.NoError(t, eng.store.Append(ctx, testEvent(
		MemoryScopeSession,
		MemoryKindDecision,
		"durable decision after transient batch",
	)))

	callCount := 0
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: func(_ context.Context, episodes, _ string) ([]ConsolidatedEvent, error) {
			callCount++
			require.Contains(t, episodes, "durable decision after transient batch")
			return []ConsolidatedEvent{
				{Kind: MemoryKindDecision, Scope: MemoryScopeProject, Content: "durable decision retained", Confidence: 0.8, Importance: 0.6},
			}, nil
		},
		clock: fixedClock,
	})
	eng.SetConsolidator(con)

	require.NoError(t, eng.TriggerConsolidation(ctx))
	require.Equal(t, 0, callCount)
	require.Equal(t, int64(500), eng.lastConsolidatedWatermark)

	require.NoError(t, eng.TriggerConsolidation(ctx))
	require.Equal(t, 1, callCount)
	require.Equal(t, int64(501), eng.lastConsolidatedWatermark)
}

func TestEngine_TriggerConsolidationDisabled(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: false})
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: func(_ context.Context, _, _ string) ([]ConsolidatedEvent, error) {
			t.Error("should not be called when engine is disabled")
			return nil, nil
		},
	})
	eng.SetConsolidator(con)

	err := eng.TriggerConsolidation(context.Background())
	require.NoError(t, err)
}

func TestEngine_TriggerConsolidationAppendsToStore(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})
	ctx := context.Background()

	evt := testEvent(MemoryScopeSession, MemoryKindProcedure, "deploy steps")
	err := eng.store.Append(ctx, evt)
	require.NoError(t, err)

	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: defaultConsolidateFn(t, []ConsolidatedEvent{
			{
				Kind:       MemoryKindProcedure,
				Scope:      MemoryScopeProject,
				Content:    "Standard deploy: build, test, push",
				Summary:    "Deploy procedure",
				Confidence: 0.85,
				Importance: 0.75,
			},
		}),
		clock: fixedClock,
	})
	eng.SetConsolidator(con)

	err = eng.TriggerConsolidation(ctx)
	require.NoError(t, err)

	// Both original + consolidated events should be queryable
	all, err := eng.store.Query(ctx, EventFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, all, 2, "store should contain both source and consolidated events")
}

func TestEngine_OnSessionClosedTriggersConsolidation(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})
	ctx := context.Background()

	// Add an event from the session
	evt := testEvent(MemoryScopeSession, MemoryKindDecision, "use sqlite")
	evt.Source.SessionID = "sess-close-test"
	err := eng.store.Append(ctx, evt)
	require.NoError(t, err)

	// Register session state
	err = eng.OnSessionCreated(ctx, "sess-close-test")
	require.NoError(t, err)

	callCount := 0
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: func(_ context.Context, _, _ string) ([]ConsolidatedEvent, error) {
			callCount++
			return []ConsolidatedEvent{
				{Kind: MemoryKindDecision, Scope: MemoryScopeProject, Content: "consolidated", Confidence: 0.9, Importance: 0.8},
			}, nil
		},
		clock: fixedClock,
	})
	eng.SetConsolidator(con)

	err = eng.OnSessionClosed(ctx, "sess-close-test")
	require.NoError(t, err)
	require.Equal(t, 1, callCount, "OnSessionClosed should trigger consolidation")
	require.NotNil(t, eng.lastConsolidationRun)
}

func TestEngine_OnSessionClosedDisabled(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: false})

	callCount := 0
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: func(_ context.Context, _, _ string) ([]ConsolidatedEvent, error) {
			callCount++
			return nil, nil
		},
	})
	eng.SetConsolidator(con)

	err := eng.OnSessionClosed(context.Background(), "sess-disabled")
	require.NoError(t, err)
	require.Equal(t, 0, callCount, "should not consolidate when engine is disabled")
}

func TestEngine_TriggerConsolidationDegradedMode(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	eng := New(db, Config{Enabled: true})
	eng.SetDegraded(true, "background model unavailable")

	evt := testEvent(MemoryScopeSession, MemoryKindDecision, "test")
	err := eng.store.Append(context.Background(), evt)
	require.NoError(t, err)

	callCount := 0
	con := newMockConsolidator(mockConsolidatorDeps{
		getExistingFn: defaultExistingFn(nil),
		analyzeFn: func(_ context.Context, _, _ string) ([]ConsolidatedEvent, error) {
			callCount++
			return nil, nil
		},
	})
	eng.SetConsolidator(con)

	err = eng.TriggerConsolidation(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, callCount, "should skip consolidation in degraded mode")
}
