package memory

import (
	"context"
	"fmt"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/stretchr/testify/require"
)

// TestLocalBackend_StatusCountsBeyondQueryDefaultLimit is a regression test
// for review finding A2: LocalBackend.Status used to count events via
// len(EventStore().Query(ctx, EventFilter{Limit: 0})), but Query's default
// limit clamps Limit<=0 to 100 rows, so the reported EventCount was capped at
// 100 regardless of how many events actually existed. Status now uses
// EventStore().Count, which issues a SQL COUNT(*) instead of loading rows.
func TestLocalBackend_StatusCountsBeyondQueryDefaultLimit(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	eng := engine.New(conn, engine.Config{Enabled: true, Backend: "local"})
	store := eng.EventStore()

	const total = 150
	for i := 0; i < total; i++ {
		err := store.Append(t.Context(), engine.MemoryEvent{
			ID:      fmt.Sprintf("evt-%d", i),
			Scope:   engine.MemoryScopeProject,
			Kind:    engine.MemoryKindReference,
			Content: fmt.Sprintf("reference doc %d", i),
			Source:  engine.MemorySourceRef{SessionID: fmt.Sprintf("sess-%d", i)},
		})
		require.NoError(t, err)
	}

	backend := NewLocalBackend(eng)
	status, err := backend.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(total), status.EventCount, "Status must count all events, not just the first Query page")
}
