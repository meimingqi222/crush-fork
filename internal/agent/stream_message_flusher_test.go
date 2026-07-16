package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/guimetrics"
	"github.com/stretchr/testify/require"
)

type flushMetricRecorder struct {
	observed atomic.Int32
	outcome  atomic.Value
}

func (r *flushMetricRecorder) ObserveDuration(name guimetrics.Name, _ time.Duration, labels guimetrics.Labels) {
	if name == guimetrics.SQLiteFlushDuration {
		r.observed.Add(1)
		r.outcome.Store(labels.Outcome)
	}
}

func (*flushMetricRecorder) Add(guimetrics.Name, int64, guimetrics.Labels)      {}
func (*flushMetricRecorder) SetGauge(guimetrics.Name, int64, guimetrics.Labels) {}

func TestStreamMessageFlusherCoalescesFlushes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var flushes atomic.Int32
	f := newStreamMessageFlusher(ctx, func() error {
		flushes.Add(1)
		return nil
	})
	defer f.Stop()

	f.MarkDirty()
	f.MarkDirty()
	f.MarkDirty()
	time.Sleep(streamMessageFlushInterval + 50*time.Millisecond)
	require.GreaterOrEqual(t, int(flushes.Load()), 1)
}

func TestStreamMessageFlusherFlushNow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var flushes atomic.Int32
	f := newStreamMessageFlusher(ctx, func() error {
		flushes.Add(1)
		return nil
	})
	defer f.Stop()

	// FlushNow is an explicit flush request — it must always flush,
	// regardless of the dirty flag. Callers like OnToolCall mutate
	// currentAssistant in memory then call flushAssistant() → FlushNow()
	// without a preceding MarkDirty(); skipping the flush would drop
	// tool-call updates when no text/reasoning delta preceded them.
	require.NoError(t, f.FlushNow())
	require.Equal(t, int32(1), flushes.Load())
	require.NoError(t, f.FlushNow())
	require.Equal(t, int32(2), flushes.Load())

	f.MarkDirty()
	require.NoError(t, f.FlushNow())
	require.Equal(t, int32(3), flushes.Load())
}

func TestStreamMessageFlusherKeepsDirtyOnFlushError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var flushes atomic.Int32
	f := newStreamMessageFlusher(ctx, func() error {
		flushes.Add(1)
		return errors.New("db down")
	})
	defer f.Stop()

	f.MarkDirty()
	err := f.FlushNow()
	require.Error(t, err)
	require.Equal(t, int32(1), flushes.Load())

	require.Error(t, f.FlushNow())
	require.Equal(t, int32(2), flushes.Load())
}

func TestStreamMessageFlusherRecordsPersistenceDuration(t *testing.T) {
	t.Parallel()

	recorder := &flushMetricRecorder{}
	ctx := guimetrics.WithRecorder(context.Background(), recorder)
	f := newStreamMessageFlusher(ctx, func() error { return nil })
	defer f.Stop()

	require.NoError(t, f.FlushNow())
	require.Equal(t, int32(1), recorder.observed.Load())
	require.Equal(t, "success", recorder.outcome.Load())
}
