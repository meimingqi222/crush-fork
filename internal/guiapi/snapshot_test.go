package guiapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/stretchr/testify/require"
)

func TestSessionSnapshotAndExpiredSyncUseBoundedSource(t *testing.T) {
	t.Parallel()

	hub := sessionevent.NewHub(sessionevent.Config{JournalEvents: 1})
	for range 2 {
		_, err := hub.Publish("session-1", sessionevent.NewEvent{Kind: sessionevent.KindSessionUpdated})
		require.NoError(t, err)
	}
	source := &fixedSnapshotSource{snapshot: sessionevent.Snapshot{
		Session:         sessionevent.SessionSummary{ID: "session-1", Title: "Snapshot"},
		Status:          "idle",
		Messages:        []sessionevent.MessageSummary{},
		MCPServers:      []sessionevent.ResourceSummary{},
		Terminals:       []sessionevent.ResourceSummary{},
		LatestSequence:  2,
		SessionRevision: 3,
	}}
	service := negotiatedSessionSyncService(t, hub, &recordingWriter{})
	service.SetSnapshotSource(source)

	result, rpcErr := service.HandleExtension(t.Context(), "crush/session/snapshot", mustRawJSON(t, snapshotParams{SessionID: "session-1"}))
	require.Nil(t, rpcErr)
	require.Equal(t, source.snapshot, result.(sessionevent.Snapshot))

	result, rpcErr = service.HandleExtension(t.Context(), "crush/session/sync", mustRawJSON(t, syncParams{
		SessionID:     "session-1",
		AfterSequence: 0,
	}))
	require.Nil(t, rpcErr)
	sync := result.(syncResult)
	require.Equal(t, "snapshot", sync.Mode)
	require.Equal(t, uint64(2), sync.LatestSequence)
	require.Equal(t, &source.snapshot, sync.Snapshot)
	require.Empty(t, sync.Events)
	require.Equal(t, 2, source.calls)

	raw, err := json.Marshal(sync)
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"events"`)
	require.Contains(t, string(raw), `"snapshot"`)
}

func TestSessionSnapshotMapsNotFoundAndSourceFailure(t *testing.T) {
	t.Parallel()

	service := negotiatedSessionSyncService(t, sessionevent.NewHub(sessionevent.Config{}), &recordingWriter{})
	source := &fixedSnapshotSource{err: sql.ErrNoRows}
	service.SetSnapshotSource(source)
	_, rpcErr := service.HandleExtension(t.Context(), "crush/session/snapshot", mustRawJSON(t, snapshotParams{SessionID: "missing"}))
	require.Equal(t, -32021, rpcErr.Code)
	require.Equal(t, errorSessionNotFound, rpcErr.Message)
	require.Equal(t, "missing", rpcErr.Data.(ErrorData).Details["sessionId"])

	source.err = errors.New("database unavailable")
	_, rpcErr = service.HandleExtension(t.Context(), "crush/session/snapshot", mustRawJSON(t, snapshotParams{SessionID: "session-1"}))
	require.Equal(t, acp.CodeInternalError, rpcErr.Code)
	require.Equal(t, errorSnapshotFailed, rpcErr.Message)
	require.Equal(t, errorSnapshotFailed, rpcErr.Data.(ErrorData).Code)
	require.True(t, rpcErr.Data.(ErrorData).Retryable)
	require.NotContains(t, rpcErr.Message, "database unavailable")
}

func TestCoordinatorSnapshotSource(t *testing.T) {
	t.Parallel()

	source := NewCoordinatorSnapshotSource(fakeSnapshotCoordinator{
		busy:         true,
		activeTurnID: "turn-1",
		queued:       2,
		paused:       true,
		model: agent.Model{ModelCfg: config.SelectedModel{
			Model:    "model-1",
			Provider: "provider-1",
		}},
		effective: session.EffectiveInference{InferenceOverrides: session.InferenceOverrides{
			Model: "model-1", Provider: "provider-1", MaxOutputTokens: pointer(int64(4096)),
			Temperature: pointer(0.7), TopP: pointer(0.8), TopK: pointer(int64(40)),
			FrequencyPenalty: pointer(0.1), PresencePenalty: pointer(0.2), Think: pointer(true),
		}, Revision: 9},
	})
	projection := source.SnapshotRuntime("session-1")
	require.True(t, projection.Busy)
	require.Equal(t, "turn-1", projection.ActiveTurnID)
	require.Equal(t, 2, projection.QueueCount)
	require.True(t, projection.QueuePaused)
	require.Equal(t, "model-1", projection.Model)
	require.Equal(t, "provider-1", projection.Provider)
	require.Equal(t, uint64(9), projection.Inference.Revision)
	require.Equal(t, int64(4096), *projection.Inference.MaxOutputTokens)
	require.Equal(t, 0.7, *projection.Inference.Temperature)
	require.Equal(t, 0.8, *projection.Inference.TopP)
	require.Equal(t, int64(40), *projection.Inference.TopK)
	require.Equal(t, 0.1, *projection.Inference.FrequencyPenalty)
	require.Equal(t, 0.2, *projection.Inference.PresencePenalty)
	require.True(t, *projection.Inference.Think)
}

type fixedSnapshotSource struct {
	snapshot sessionevent.Snapshot
	err      error
	calls    int
}

func (s *fixedSnapshotSource) Snapshot(context.Context, string) (sessionevent.Snapshot, error) {
	s.calls++
	return s.snapshot, s.err
}

type fakeSnapshotCoordinator struct {
	busy         bool
	activeTurnID string
	queued       int
	paused       bool
	model        agent.Model
	effective    session.EffectiveInference
}

func (c fakeSnapshotCoordinator) IsSessionBusy(string) bool  { return c.busy }
func (c fakeSnapshotCoordinator) ActiveTurnID(string) string { return c.activeTurnID }
func (c fakeSnapshotCoordinator) QueuedPrompts(string) int   { return c.queued }
func (c fakeSnapshotCoordinator) IsQueuePaused(string) bool  { return c.paused }
func (c fakeSnapshotCoordinator) ModelForSession(string) (agent.Model, bool) {
	return c.model, true
}

func (c fakeSnapshotCoordinator) EffectiveInference(context.Context, string) (session.EffectiveInference, error) {
	return c.effective, nil
}

func pointer[T any](value T) *T { return &value }
