package agent

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/stretchr/testify/require"
)

func TestSplitLiveTextBoundsUTF8Chunks(t *testing.T) {
	t.Parallel()

	input := strings.Repeat("界", maxLiveDeltaBytes)
	chunks := splitLiveText(input)
	require.Greater(t, len(chunks), 1)
	var rebuilt strings.Builder
	for _, chunk := range chunks {
		require.LessOrEqual(t, len(chunk), maxLiveDeltaBytes)
		require.True(t, utf8.ValidString(chunk))
		rebuilt.WriteString(chunk)
	}
	require.Equal(t, input, rebuilt.String())
}

func TestBoundedLiveToolTextPreservesUTF8(t *testing.T) {
	t.Parallel()

	result, truncated := boundedLiveToolText(strings.Repeat("界", maxLiveToolTextBytes))
	require.True(t, truncated)
	require.LessOrEqual(t, len(result), maxLiveToolTextBytes)
	require.True(t, utf8.ValidString(result))
}

func BenchmarkLiveTextDeltaPublish(b *testing.B) {
	hub := sessionevent.NewHub(sessionevent.Config{})
	defer hub.Close()
	agent := &sessionAgent{sessionEvents: hub}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		agent.publishLiveTextDelta(ctx, "benchmark-session", time.Now(), sessionevent.KindMessageDelta, "message", "part", "representative provider delta")
	}
}

func BenchmarkLiveTextDeltaPublishSubscribed(b *testing.B) {
	hub := sessionevent.NewHub(sessionevent.Config{})
	defer hub.Close()
	subscription, err := hub.Subscribe("benchmark-session", 0)
	if err != nil {
		b.Fatal(err)
	}
	defer subscription.Close()
	agent := &sessionAgent{sessionEvents: hub}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		agent.publishLiveTextDelta(ctx, "benchmark-session", time.Now(), sessionevent.KindMessageDelta, "message", "part", "representative provider delta")
	}
}
