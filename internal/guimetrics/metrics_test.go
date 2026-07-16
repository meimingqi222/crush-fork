package guimetrics

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type recordingRecorder struct {
	mu       sync.Mutex
	observed []Name
}

func (r *recordingRecorder) ObserveDuration(name Name, _ time.Duration, _ Labels) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed = append(r.observed, name)
}

func (*recordingRecorder) Add(Name, int64, Labels)      {}
func (*recordingRecorder) SetGauge(Name, int64, Labels) {}

func TestRecorderRoundTripsThroughContext(t *testing.T) {
	t.Parallel()

	recorder := &recordingRecorder{}
	ctx := WithRecorder(context.Background(), recorder)
	FromContext(ctx).ObserveDuration(ACPRequestDuration, time.Millisecond, Labels{Method: "initialize"})

	require.Equal(t, []Name{ACPRequestDuration}, recorder.observed)
}

func TestLabelsHaveNoOpenEndedMap(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeFor[Labels]()
	for i := range typ.NumField() {
		field := typ.Field(i)
		require.NotEqual(t, reflect.Map, field.Type.Kind(), "label %s permits unbounded keys", field.Name)
		require.Equal(t, reflect.String, field.Type.Kind(), "label %s must be a bounded enum string", field.Name)
	}
}

func TestNoopRecorderIsSafe(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		recorder := FromContext(context.Background())
		recorder.ObserveDuration(SQLiteFlushDuration, time.Second, Labels{})
		recorder.Add(GUIEventCoalescedTotal, 1, Labels{})
		recorder.SetGauge(ActivePromptCount, 0, Labels{})
	})
}
