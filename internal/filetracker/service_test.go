package filetracker

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

type testEnv struct {
	ctx context.Context
	q   *db.Queries
	svc *service
}

func setupTest(t *testing.T) *testEnv {
	t.Helper()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	q := db.New(conn)
	s := NewService(q).(*service)
	t.Cleanup(func() { s.Close() })
	return &testEnv{
		ctx: t.Context(),
		q:   q,
		svc: s,
	}
}

func (e *testEnv) createSession(t *testing.T, sessionID string) {
	t.Helper()
	_, err := e.q.CreateSession(e.ctx, db.CreateSessionParams{
		ID:    sessionID,
		Kind:  string(session.KindNormal),
		Title: "Test Session",
	})
	require.NoError(t, err)
}

// flushWait polls LastReadTime until it returns a non-zero value or times out.
func (e *testEnv) flushWait(t *testing.T, sessionID, path string) time.Time {
	t.Helper()
	var result time.Time
	require.Eventually(t, func() bool {
		result = e.svc.LastReadTime(e.ctx, sessionID, path)
		return !result.IsZero()
	}, 2*time.Second, 10*time.Millisecond)
	return result
}

func TestService_RecordRead(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-1"
	path := "/path/to/file.go"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, path)

	lastRead := env.flushWait(t, sessionID, path)
	require.WithinDuration(t, time.Now(), lastRead, 5*time.Second)
}

func TestService_LastReadTime_NotFound(t *testing.T) {
	env := setupTest(t)

	lastRead := env.svc.LastReadTime(env.ctx, "nonexistent-session", "/nonexistent/path")
	require.True(t, lastRead.IsZero(), "expected zero time for unread file")
}

func TestService_RecordRead_UpdatesTimestamp(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-2"
	path := "/path/to/file.go"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, path)
	firstRead := env.flushWait(t, sessionID, path)
	require.False(t, firstRead.IsZero())

	synctest.Test(t, func(t *testing.T) {
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		env.svc.RecordRead(env.ctx, sessionID, path)
		secondRead := env.flushWait(t, sessionID, path)

		require.False(t, secondRead.Before(firstRead), "second read time should not be before first")
	})
}

func TestService_RecordRead_DifferentSessions(t *testing.T) {
	env := setupTest(t)

	path := "/shared/file.go"
	session1, session2 := "session-1", "session-2"
	env.createSession(t, session1)
	env.createSession(t, session2)

	env.svc.RecordRead(env.ctx, session1, path)
	lastRead1 := env.flushWait(t, session1, path)
	require.False(t, lastRead1.IsZero())

	lastRead2 := env.svc.LastReadTime(env.ctx, session2, path)
	require.True(t, lastRead2.IsZero(), "session 2 should not see session 1's read")
}

func TestService_RecordRead_DifferentPaths(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-3"
	path1, path2 := "/path/to/file1.go", "/path/to/file2.go"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, path1)
	lastRead1 := env.flushWait(t, sessionID, path1)
	require.False(t, lastRead1.IsZero())

	lastRead2 := env.svc.LastReadTime(env.ctx, sessionID, path2)
	require.True(t, lastRead2.IsZero(), "path2 should not be recorded")
}

// TestService_RecordReadAfterClose verifies that RecordRead never panics or
// blocks after Close, even though the write channel is not closed.
func TestService_RecordReadAfterClose(t *testing.T) {
	env := setupTest(t)
	env.createSession(t, "test-session-close")

	env.svc.RecordRead(env.ctx, "test-session-close", "/path/to/file.go")
	require.NotPanics(t, func() {
		env.svc.Close()
		// Records after close must be dropped, not panic (send on closed
		// channel) and not block.
		env.svc.RecordRead(env.ctx, "test-session-close", "/path/to/after-close.go")
	})
	// Close is idempotent.
	require.NotPanics(t, func() { env.svc.Close() })
}

// TestService_ConcurrentRecordAndRead exercises concurrent RecordRead and
// read queries to shake out races in the write loop and syncFlush.
func TestService_ConcurrentRecordAndRead(t *testing.T) {
	env := setupTest(t)
	env.createSession(t, "test-session-conc")

	done := make(chan struct{})
	defer close(done)

	const writers, readers = 4, 4
	for w := 0; w < writers; w++ {
		go func(w int) {
			for i := 0; i < 200; i++ {
				path := fmt.Sprintf("/path/to/w%d/f%d.go", w, i%16)
				env.svc.RecordRead(env.ctx, "test-session-conc", path)
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		go func() {
			for i := 0; i < 200; i++ {
				path := fmt.Sprintf("/path/to/w%d/f%d.go", r, i%16)
				_ = env.svc.LastReadTime(env.ctx, "test-session-conc", path)
				_, _ = env.svc.ListReadFiles(env.ctx, "test-session-conc")
			}
		}()
	}

	// Give the goroutines time to finish before t.Cleanup closes the
	// service. Use a barrier that waits for all writes to be visible.
	require.Eventually(t, func() bool {
		env.svc.syncFlush()
		files, err := env.svc.ListReadFiles(env.ctx, "test-session-conc")
		return err == nil && len(files) > 0
	}, 5*time.Second, 20*time.Millisecond)
}
