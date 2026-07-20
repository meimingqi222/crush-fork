package guiapi

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/idempotency"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSessionMutationHandlersReplayExactResultsAndPublishMetadata(t *testing.T) {
	t.Parallel()

	env := newSessionMutationEnvironment(t)
	requestID := uuid.NewString()
	rename := sessionRenameParams{SessionID: env.source.ID, Title: "Renamed", ClientRequestID: requestID}
	first, rpcErr := env.gui.HandleExtension(t.Context(), "crush/session/rename", mustRawJSON(t, rename))
	require.Nil(t, rpcErr)
	replayed, rpcErr := env.gui.HandleExtension(t.Context(), "crush/session/rename", mustRawJSON(t, rename))
	require.Nil(t, rpcErr)
	require.Equal(t, first, replayed)
	require.Equal(t, "Renamed", first.(sessionevent.SessionSummary).Title)

	rename.Title = "Conflict"
	_, rpcErr = env.gui.HandleExtension(t.Context(), "crush/session/rename", mustRawJSON(t, rename))
	require.Equal(t, errorIdempotencyConflict, rpcErr.Message)

	archived, rpcErr := env.gui.HandleExtension(t.Context(), "crush/session/archive", mustRawJSON(t, sessionArchiveParams{
		SessionID: env.source.ID, Archived: true, ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	require.True(t, archived.(sessionevent.SessionSummary).Archived)
	pinned, rpcErr := env.gui.HandleExtension(t.Context(), "crush/session/pin", mustRawJSON(t, sessionPinParams{
		SessionID: env.source.ID, Pinned: true, ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	require.True(t, pinned.(sessionevent.SessionSummary).Pinned)
	disconnected, cancel := context.WithCancel(t.Context())
	cancel()
	archived, rpcErr = env.gui.HandleExtension(disconnected, "crush/session/archive", mustRawJSON(t, sessionArchiveParams{
		SessionID: env.source.ID, Archived: false, ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	require.False(t, archived.(sessionevent.SessionSummary).Archived)

	got, rpcErr := env.gui.HandleExtension(t.Context(), "crush/session/get", mustRawJSON(t, sessionIDParams{SessionID: env.source.ID}))
	require.Nil(t, rpcErr)
	get := got.(sessionGetResult)
	summary := get.Session
	require.Equal(t, "Renamed", summary.Title)
	require.False(t, summary.Archived)
	require.True(t, summary.Pinned)
	require.Equal(t, uint64(4), get.LatestSequence)
	require.Equal(t, uint64(4), get.SessionRevision)

	events, err := env.hub.ReplayAfter(env.source.ID, 0)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(events), 4)
	require.Equal(t, sessionevent.KindSessionUpdated, events[len(events)-1].Kind)
	require.Equal(t, uint64(4), events[len(events)-1].SessionRevision)
	require.Equal(t, uint64(4), env.hub.LatestRevision(env.source.ID))
}

func TestSessionForkIsBoundedIdempotentAndReturnsSnapshot(t *testing.T) {
	t.Parallel()

	env := newSessionMutationEnvironment(t)
	firstUser := createGUIMessage(t, env.messages, env.source.ID, message.User, "first")
	createGUIMessage(t, env.messages, env.source.ID, message.Assistant, "first answer")
	createGUIMessage(t, env.messages, env.source.ID, message.User, "second")

	params := sessionForkParams{SessionID: env.source.ID, MessageID: firstUser.ID, ClientRequestID: uuid.NewString()}
	result, rpcErr := env.gui.HandleExtension(t.Context(), "crush/session/fork", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	forked := result.(sessionForkResult)
	require.NotEqual(t, env.source.ID, forked.SessionID)
	require.Equal(t, forked.SessionID, forked.Snapshot.Session.ID)
	require.Len(t, forked.Snapshot.Messages, 2)

	replayed, rpcErr := env.gui.HandleExtension(t.Context(), "crush/session/fork", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	require.Equal(t, forked.SessionID, replayed.(sessionForkResult).SessionID)
	all, err := env.sessions.List(t.Context())
	require.NoError(t, err)
	require.Len(t, all, 2)

	_, rpcErr = env.gui.HandleExtension(t.Context(), "crush/session/fork", mustRawJSON(t, sessionForkParams{
		SessionID: env.source.ID, MessageID: "missing", ClientRequestID: uuid.NewString(),
	}))
	require.Equal(t, errorForkBoundaryInvalid, rpcErr.Message)
}

func TestActiveSessionDeleteTearsDownBeforeDeleteAndReplaysAfterRemoval(t *testing.T) {
	t.Parallel()

	env := newSessionMutationEnvironment(t)
	requestID := uuid.NewString()
	params := sessionDeleteParams{SessionID: env.source.ID, ClientRequestID: requestID}
	first, rpcErr := env.gui.HandleExtension(t.Context(), "crush/session/delete", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	require.True(t, first.(sessionDeleteResult).Deleted)
	require.Equal(t, []string{"runtime", "delete"}, env.lifecycle.values())

	replayed, rpcErr := env.gui.HandleExtension(t.Context(), "crush/session/delete", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	require.Equal(t, first, replayed)
	_, err := env.sessions.Get(t.Context(), env.source.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.Equal(t, []string{"runtime", "delete"}, env.lifecycle.values())
	_, rpcErr = env.gui.HandleExtension(t.Context(), "crush/session/delete", mustRawJSON(t, sessionDeleteParams{
		SessionID: env.source.ID, ClientRequestID: uuid.NewString(),
	}))
	require.Equal(t, errorSessionNotFound, rpcErr.Message)
	require.Equal(t, []string{"runtime", "delete"}, env.lifecycle.values())
}

func TestSessionInferenceHandlersResolvePrecedenceAndRejectStaleRevision(t *testing.T) {
	t.Parallel()

	env := newSessionMutationEnvironment(t)
	initial, rpcErr := env.gui.HandleExtension(t.Context(), "crush/session/config/get", mustRawJSON(t, sessionInferenceGetParams{SessionID: env.source.ID}))
	require.Nil(t, rpcErr)
	require.Zero(t, initial.(sessionInferenceResult).Revision)
	require.Equal(t, "global-model", initial.(sessionInferenceResult).Effective.Model)

	temperature := 0.7
	params := sessionInferenceUpdateParams{
		SessionID: env.source.ID, ExpectedRevision: 0,
		Overrides: session.InferenceOverrides{Temperature: &temperature}, ClientRequestID: uuid.NewString(),
	}
	updated, rpcErr := env.gui.HandleExtension(t.Context(), "crush/session/config/update", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	result := updated.(sessionInferenceResult)
	require.Equal(t, uint64(1), result.Revision)
	require.Equal(t, 0.7, *result.Effective.Temperature)
	require.Equal(t, "global-model", result.Effective.Model)

	replayed, rpcErr := env.gui.HandleExtension(t.Context(), "crush/session/config/update", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	require.Equal(t, result, replayed)
	params.ClientRequestID = uuid.NewString()
	_, rpcErr = env.gui.HandleExtension(t.Context(), "crush/session/config/update", mustRawJSON(t, params))
	require.Equal(t, errorRevisionConflict, rpcErr.Message)

	params.ExpectedRevision = 1
	params.Overrides = session.InferenceOverrides{Model: "bad", Provider: "provider"}
	params.ClientRequestID = uuid.NewString()
	_, rpcErr = env.gui.HandleExtension(t.Context(), "crush/session/config/update", mustRawJSON(t, params))
	require.Equal(t, acp.CodeInvalidParams, rpcErr.Code)

	params.ExpectedRevision = 1
	params.Overrides = session.InferenceOverrides{}
	params.ClientRequestID = uuid.NewString()
	cleared, rpcErr := env.gui.HandleExtension(t.Context(), "crush/session/config/update", mustRawJSON(t, params))
	require.Nil(t, rpcErr)
	clearedResult := cleared.(sessionInferenceResult)
	require.Equal(t, uint64(2), clearedResult.Revision)
	require.Equal(t, session.InferenceOverrides{}, clearedResult.Overrides)
	require.Equal(t, "global-model", clearedResult.Effective.Model)
	require.Nil(t, clearedResult.Effective.Temperature)

	_, rpcErr = env.gui.HandleExtension(t.Context(), "crush/session/config/get", mustRawJSON(t, sessionInferenceGetParams{SessionID: "missing"}))
	require.Equal(t, errorSessionNotFound, rpcErr.Message)
	_, rpcErr = env.gui.HandleExtension(t.Context(), "crush/session/config/update", mustRawJSON(t, sessionInferenceUpdateParams{
		SessionID: "missing", ClientRequestID: uuid.NewString(),
	}))
	require.Equal(t, errorSessionNotFound, rpcErr.Message)
}

func TestSessionSummaryTimestampDTOsUseMilliseconds(t *testing.T) {
	t.Parallel()

	summary := sessionSummary(session.Session{CreatedAt: 1_783_910_000, UpdatedAt: 1_783_910_123})
	require.Equal(t, int64(1_783_910_000_000), summary.CreatedAt)
	require.Equal(t, int64(1_783_910_123_000), summary.UpdatedAt)
}

type sessionMutationEnvironment struct {
	gui       *Service
	hub       *sessionevent.Hub
	sessions  session.Service
	messages  message.Service
	source    session.Session
	lifecycle *lifecycleRecorder
}

func newSessionMutationEnvironment(t *testing.T) *sessionMutationEnvironment {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	queries := db.New(conn)
	lifecycle := &lifecycleRecorder{}
	sessions := session.NewServiceWithDeleteCallback(queries, conn, func(string) { lifecycle.add("delete") })
	messages := message.NewService(queries)
	source, err := sessions.Create(t.Context(), "Source")
	require.NoError(t, err)
	hub := sessionevent.NewHub(sessionevent.Config{})
	store := idempotency.New(idempotency.Config{})
	gui := NewService(hub)
	gui.SetSessionContentSources(sessions, messages, nil)
	gui.SetSessionMutationServices(sessions, lifecycleRuntime{recorder: lifecycle})
	gui.SetInferenceResolver(testInferenceResolver{sessions: sessions})
	gui.SetTurnServices(nil, store)
	gui.SetSnapshotSource(sessionevent.NewSnapshotService(sessions, messages, nil, hub))
	require.Nil(t, gui.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureSessionSync, FeatureSessionControl},
	})))
	t.Cleanup(func() {
		gui.Close()
		store.Close()
		hub.Close()
	})
	return &sessionMutationEnvironment{gui: gui, hub: hub, sessions: sessions, messages: messages, source: source, lifecycle: lifecycle}
}

type lifecycleRecorder struct {
	mu    sync.Mutex
	items []string
}

func (r *lifecycleRecorder) add(value string) {
	r.mu.Lock()
	r.items = append(r.items, value)
	r.mu.Unlock()
}

func (r *lifecycleRecorder) values() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.items...)
}

type lifecycleRuntime struct{ recorder *lifecycleRecorder }

func (r lifecycleRuntime) CloseSession(string) { r.recorder.add("runtime") }

type testInferenceResolver struct{ sessions session.Service }

func (r testInferenceResolver) ValidateInferenceOverrides(_ context.Context, _ string, overrides session.InferenceOverrides) error {
	if overrides.Model == "bad" {
		return errors.New("unknown model")
	}
	return nil
}

func (r testInferenceResolver) EffectiveInference(ctx context.Context, sessionID string) (session.EffectiveInference, error) {
	value, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.EffectiveInference{}, err
	}
	maxTokens := int64(8192)
	result := session.EffectiveInference{InferenceOverrides: session.InferenceOverrides{
		Model: "global-model", Provider: "provider", MaxOutputTokens: &maxTokens,
	}, Revision: value.InferenceRevision}
	if value.Inference.Model != "" {
		result.Model, result.Provider = value.Inference.Model, value.Inference.Provider
	}
	if value.Inference.MaxOutputTokens != nil {
		result.MaxOutputTokens = value.Inference.MaxOutputTokens
	}
	if value.Inference.Temperature != nil {
		result.Temperature = value.Inference.Temperature
	}
	return result, nil
}

func createGUIMessage(t *testing.T, service message.Service, sessionID string, role message.MessageRole, text string) message.Message {
	t.Helper()
	created, err := service.Create(context.Background(), sessionID, message.CreateMessageParams{
		Role: role, Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	require.NoError(t, err)
	return created
}
