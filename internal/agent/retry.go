package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/httpext"
)

// streamIdleTimeout is the maximum time to wait between stream parts before
// considering the connection stalled and triggering a retry. Overridable via
// the CRUSH_STREAM_IDLE_TIMEOUT env var (e.g. "30s").
var streamIdleTimeout = envDuration("CRUSH_STREAM_IDLE_TIMEOUT", 45*time.Second)

// streamConnectTimeout is the maximum time to wait for the first stream part
// before considering the connection stalled. Slower than the inter-part idle
// timeout so a slow model that eventually produces a first token is not
// killed prematurely. Overridable via CRUSH_STREAM_CONNECT_TIMEOUT.
var streamConnectTimeout = envDuration("CRUSH_STREAM_CONNECT_TIMEOUT", 90*time.Second)

// envDuration returns the value of the named env var parsed as a duration,
// or fallback when unset/invalid.
func envDuration(name string, fallback time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

// retryableStreamModel wraps a fantasy.LanguageModel and converts bare
// retryable network errors (such as io.ErrUnexpectedEOF) inside stream parts
// into *fantasy.ProviderError so the fantasy library's built-in retry
// mechanism can recognize and retry them.
type retryableStreamModel struct {
	fantasy.LanguageModel
}

// Stream implements fantasy.LanguageModel.
func (m retryableStreamModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	// Create a derived context with cancel to ensure the underlying stream
	// can be stopped when idle timeout fires or the outer function returns.
	localCtx, localCancel := context.WithCancel(ctx)

	// Attach a stream-activity channel so that HTTP response-body reads
	// (including SSE keep-alive pings silently consumed by the SDK) reset
	// the idle timer. Without this, ping events never produce StreamParts
	// and the timer fires even though the network connection is alive.
	localCtx, activityCh := httpext.WithStreamActivity(localCtx)

	stream, err := m.LanguageModel.Stream(localCtx, call)
	if err != nil {
		localCancel()
		return nil, wrapRetryableNetworkErr(err)
	}

	return func(yield func(fantasy.StreamPart) bool) {
		sawToolUse := false
		sawFirstPart := false
		// idleTimer tracks time between stream parts to detect stalled
		// connections. Before the first part arrives it uses the (longer)
		// connect timeout; afterwards it uses the inter-part idle timeout.
		idleTimer := time.NewTimer(streamConnectTimeout)
		defer idleTimer.Stop()
		defer localCancel()

		// Create a channel to receive stream parts from the underlying stream.
		// This allows us to use select with a timeout.
		partCh := make(chan fantasy.StreamPart)
		doneCh := make(chan struct{})

		// Run the underlying stream in a goroutine.
		go func() {
			defer close(doneCh)
			stream(func(part fantasy.StreamPart) bool {
				// Check if context is cancelled.
				select {
				case <-localCtx.Done():
					return false
				default:
				}

				// Send part to channel, respecting context cancellation.
				select {
				case partCh <- part:
					return true
				case <-localCtx.Done():
					return false
				}
			})
		}()

		for {
			select {
			case part := <-partCh:
				// Reset the idle timer on each part received. The initial
				// timer uses the longer connect timeout; once the first
				// part arrives we switch to the shorter inter-part idle
				// timeout so a stall between parts is caught sooner.
				if !sawFirstPart {
					sawFirstPart = true
				}
				resetTimer(idleTimer, streamIdleTimeout)

				if isToolStreamPart(part.Type) {
					sawToolUse = true
				}
				if !sawToolUse && part.Type == fantasy.StreamPartTypeError && part.Error != nil {
					part.Error = wrapRetryableNetworkErr(part.Error)
				}
				if !yield(part) {
					return
				}

			case <-activityCh:
				// HTTP response body received data (e.g. SSE ping events
				// that the SDK consumes without yielding StreamParts).
				// Reset the idle timer to prevent false timeouts. Before the
				// first part arrives we keep the (longer) connect timeout.
				if !sawFirstPart {
					resetTimer(idleTimer, streamConnectTimeout)
				} else {
					resetTimer(idleTimer, streamIdleTimeout)
				}

			case <-idleTimer.C:
				// Stream has been idle for too long - trigger a retryable error.
				// Report the actual timeout that fired so the message is truthful.
				timeout := streamIdleTimeout
				if !sawFirstPart {
					timeout = streamConnectTimeout
				}
				yield(fantasy.StreamPart{
					Type: fantasy.StreamPartTypeError,
					Error: &fantasy.ProviderError{
						Title:   "network error",
						Message: streamIdleTimeoutMessage(timeout),
						Cause:   errors.New("stream idle timeout"),
					},
				})
				return

			case <-ctx.Done():
				// Context cancelled - propagate cancellation error.
				yield(fantasy.StreamPart{
					Type:  fantasy.StreamPartTypeError,
					Error: ctx.Err(),
				})
				return

			case <-doneCh:
				// Stream completed normally.
				return
			}
		}
	}, nil
}

// resetTimer safely resets a timer, draining any pending fire.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func isToolStreamPart(partType fantasy.StreamPartType) bool {
	switch partType {
	case fantasy.StreamPartTypeToolInputStart,
		fantasy.StreamPartTypeToolInputDelta,
		fantasy.StreamPartTypeToolInputEnd,
		fantasy.StreamPartTypeToolCall,
		fantasy.StreamPartTypeToolResult:
		return true
	default:
		return false
	}
}

func streamIdleTimeoutMessage(timeout time.Duration) string {
	return fmt.Sprintf(
		"stream idle timeout: no data received for %ds",
		int(timeout/time.Second),
	)
}

// wrapRetryableNetworkErr wraps known retryable network errors into
// *fantasy.ProviderError so the fantasy retry mechanism can recognize them as
// retryable. If the error is not a known retryable network error, it is
// returned unchanged.
func wrapRetryableNetworkErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return &fantasy.ProviderError{
			Title:   "network error",
			Message: err.Error(),
			Cause:   err,
		}
	}
	return err
}
