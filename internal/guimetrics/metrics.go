// Package guimetrics defines low-overhead, bounded-label measurements for the
// desktop protocol. It intentionally does not choose an exporter.
package guimetrics

import (
	"context"
	"time"
)

// Name identifies a desktop performance or resource metric.
type Name string

const (
	ACPRequestDuration         Name = "acp_request_duration"
	GUIEventQueueDepth         Name = "gui_event_queue_depth"
	GUIEventCoalescedTotal     Name = "gui_event_coalesced_total"
	GUISnapshotTotal           Name = "gui_snapshot_total"
	GUISequenceGapTotal        Name = "gui_sequence_gap_total"
	SessionLoadDuration        Name = "session_load_duration"
	StreamChunkToEventDuration Name = "stream_chunk_to_event_duration"
	StreamEventToWriteDuration Name = "stream_event_to_write_duration"
	SQLiteFlushDuration        Name = "sqlite_flush_duration"
	TerminalRetainedBytes      Name = "terminal_retained_bytes"
	BlobRetainedBytes          Name = "blob_retained_bytes"
	ActiveSubscriptionCount    Name = "active_subscription_count"
	ActivePromptCount          Name = "active_prompt_count"
)

// Labels is deliberately closed instead of a map. Values MUST come from
// bounded enums; callers must never put session IDs, paths, prompt text, or
// arbitrary error strings in these fields.
type Labels struct {
	Method    string
	Kind      string
	Outcome   string
	Transport string
}

// Recorder consumes desktop measurements. Implementations must be safe for
// concurrent use and must not block protocol or provider hot paths.
type Recorder interface {
	ObserveDuration(name Name, duration time.Duration, labels Labels)
	Add(name Name, delta int64, labels Labels)
	SetGauge(name Name, value int64, labels Labels)
}

type contextKey struct{}

// WithRecorder installs recorder for request and streaming work derived from
// ctx. A nil recorder is treated as the default no-op recorder.
func WithRecorder(ctx context.Context, recorder Recorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, recorder)
}

// FromContext returns the installed recorder or a no-op recorder.
func FromContext(ctx context.Context) Recorder {
	if ctx != nil {
		if recorder, ok := ctx.Value(contextKey{}).(Recorder); ok && recorder != nil {
			return recorder
		}
	}
	return noopRecorder{}
}

type noopRecorder struct{}

func (noopRecorder) ObserveDuration(Name, time.Duration, Labels) {}
func (noopRecorder) Add(Name, int64, Labels)                     {}
func (noopRecorder) SetGauge(Name, int64, Labels)                {}
