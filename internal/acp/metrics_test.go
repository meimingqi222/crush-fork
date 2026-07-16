package acp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/guimetrics"
	"github.com/stretchr/testify/require"
)

type metricObservation struct {
	name   guimetrics.Name
	value  int64
	labels guimetrics.Labels
}

type testMetricRecorder struct {
	mu           sync.Mutex
	observations []metricObservation
}

func (r *testMetricRecorder) ObserveDuration(name guimetrics.Name, _ time.Duration, labels guimetrics.Labels) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observations = append(r.observations, metricObservation{name: name, labels: labels})
}

func (*testMetricRecorder) Add(guimetrics.Name, int64, guimetrics.Labels) {}

func (r *testMetricRecorder) SetGauge(name guimetrics.Name, value int64, labels guimetrics.Labels) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observations = append(r.observations, metricObservation{name: name, value: value, labels: labels})
}

func TestHandlerRecordsBoundedRequestMetrics(t *testing.T) {
	t.Parallel()

	recorder := &testMetricRecorder{}
	ctx := guimetrics.WithRecorder(context.Background(), recorder)
	handler := NewHandler(nil)

	_, rpcErr := handler.Handle(ctx, &Request{Method: "client/arbitrary-session-123"})
	require.NotNil(t, rpcErr)
	require.Equal(t, []metricObservation{{
		name: guimetrics.ACPRequestDuration,
		labels: guimetrics.Labels{
			Method:    "other",
			Outcome:   "error",
			Transport: "stdio",
		},
	}}, recorder.observations)
}

func TestRecordActivePromptCount(t *testing.T) {
	t.Parallel()

	recorder := &testMetricRecorder{}
	ctx := guimetrics.WithRecorder(context.Background(), recorder)

	recordActivePromptCount(ctx, 3)
	recordActivePromptCount(ctx, 0)

	require.Equal(t, []metricObservation{
		{name: guimetrics.ActivePromptCount, value: 3},
		{name: guimetrics.ActivePromptCount, value: 0},
	}, recorder.observations)
}

func TestMetricMethodKeepsOnlyKnownMethods(t *testing.T) {
	t.Parallel()

	require.Equal(t, "session/load", metricMethod("session/load"))
	require.Equal(t, "other", metricMethod("client/arbitrary-session-123"))
}

func TestTransportMetricLabelIsBounded(t *testing.T) {
	t.Parallel()

	require.Equal(t, "stdio", TransportName(context.Background()))
	require.Equal(t, "pipe", TransportName(withTransportName(context.Background(), "named_pipe")))
	require.Equal(t, "other", TransportName(withTransportName(context.Background(), "client-controlled-value")))
}
