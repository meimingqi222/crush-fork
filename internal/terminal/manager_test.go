package terminal

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManagerLifecycleOffsetsTruncationAndOwnership(t *testing.T) {
	t.Parallel()

	factory := newFakeFactory()
	var outputOffsets []uint64
	var exits []Exit
	var callbackMu sync.Mutex
	manager := New(Config{
		Factory: factory, RetainedBytes: 8, SnapshotBytes: 4,
		OnOutput: func(_ string, _ string, offset uint64, _ []byte) {
			callbackMu.Lock()
			outputOffsets = append(outputOffsets, offset)
			callbackMu.Unlock()
		},
		OnExit: func(_, _ string, exit Exit) {
			callbackMu.Lock()
			exits = append(exits, exit)
			callbackMu.Unlock()
		},
	})
	t.Cleanup(manager.Close)

	metadata, err := manager.Open(t.Context(), OpenRequest{
		ClientID: "client-a", SessionID: "session-a", Command: "fake", Cols: 80, Rows: 24,
	})
	require.NoError(t, err)
	proc := factory.latest()
	proc.emit([]byte("012345"))
	proc.emit([]byte("6789"))
	require.Eventually(t, func() bool {
		snapshot, snapshotErr := manager.Snapshot("client-a", "session-a", metadata.ID, 0)
		return snapshotErr == nil && snapshot.Metadata.Offset == 10
	}, time.Second, time.Millisecond)

	snapshot, err := manager.Snapshot("client-a", "session-a", metadata.ID, 0)
	require.NoError(t, err)
	require.True(t, snapshot.Truncated)
	require.Equal(t, uint64(2), snapshot.StartOffset)
	require.Equal(t, uint64(6), snapshot.EndOffset)
	require.Equal(t, []byte("2345"), snapshot.Data)
	require.True(t, snapshot.More)
	reconnect, err := manager.Snapshot("client-a", "session-a", metadata.ID, 6)
	require.NoError(t, err)
	require.False(t, reconnect.Truncated)
	require.Equal(t, []byte("6789"), reconnect.Data)
	require.False(t, reconnect.More)
	_, err = manager.Snapshot("client-a", "session-a", metadata.ID, 11)
	require.ErrorIs(t, err, ErrInvalidOffset)
	_, err = manager.Snapshot("client-b", "session-a", metadata.ID, 0)
	require.ErrorIs(t, err, ErrOwnerMismatch)
	_, err = manager.Snapshot("client-a", "session-b", metadata.ID, 0)
	require.ErrorIs(t, err, ErrOwnerMismatch)

	written, err := manager.Input(t.Context(), "client-a", "session-a", metadata.ID, []byte("input"))
	require.NoError(t, err)
	require.Equal(t, 5, written)
	require.Equal(t, []byte("input"), proc.writtenBytes())
	resized, err := manager.Resize("client-a", "session-a", metadata.ID, 120, 40)
	require.NoError(t, err)
	require.Equal(t, 120, resized.Cols)
	require.Equal(t, 40, resized.Rows)
	require.Equal(t, [2]int{120, 40}, proc.dimensions())

	require.NoError(t, manager.Kill("client-a", "session-a", metadata.ID, "interrupt"))
	require.Eventually(t, func() bool {
		value, snapshotErr := manager.Snapshot("client-a", "session-a", metadata.ID, 10)
		return snapshotErr == nil && value.State == StateKilled && value.HasExit
	}, time.Second, time.Millisecond)
	require.True(t, proc.isClosed())
	callbackMu.Lock()
	require.Equal(t, []uint64{0, 6}, outputOffsets)
	require.Len(t, exits, 1)
	require.Equal(t, "interrupt", exits[0].Signal)
	callbackMu.Unlock()
}

func TestManagerCapacityExpiryAndCleanup(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	factory := newFakeFactory()
	manager := New(Config{Factory: factory, MaxTerminals: 1, Retention: time.Minute, Clock: func() time.Time { return now }})
	first, err := manager.Open(t.Context(), OpenRequest{ClientID: "client", SessionID: "session", Command: "fake"})
	require.NoError(t, err)
	_, err = manager.Open(t.Context(), OpenRequest{ClientID: "client", SessionID: "session", Command: "fake"})
	require.ErrorIs(t, err, ErrCapacity)
	factory.latest().exit(ProcessExit{Code: 0})
	require.Eventually(t, func() bool {
		value, snapshotErr := manager.Snapshot("client", "session", first.ID, 0)
		return snapshotErr == nil && value.State == StateExited
	}, time.Second, time.Millisecond)
	now = now.Add(time.Minute + time.Nanosecond)
	second, err := manager.Open(t.Context(), OpenRequest{ClientID: "client", SessionID: "session", Command: "fake"})
	require.NoError(t, err)
	_, err = manager.Snapshot("client", "session", first.ID, 0)
	require.ErrorIs(t, err, ErrNotFound)
	factory.latest().emit([]byte("retained"))
	require.Eventually(t, func() bool { return manager.RetainedBytes() > 0 }, time.Second, time.Millisecond)
	manager.CloseClient("client")
	_, err = manager.Snapshot("client", "session", second.ID, 0)
	require.ErrorIs(t, err, ErrNotFound)
	require.Zero(t, manager.RetainedBytes())
	manager.Close()
	_, err = manager.Open(t.Context(), OpenRequest{ClientID: "client", SessionID: "session", Command: "fake"})
	require.ErrorIs(t, err, ErrClosed)
}

func TestManagerRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	manager := New(Config{Factory: newFakeFactory()})
	t.Cleanup(manager.Close)
	clamped := New(Config{Factory: newFakeFactory(), RetainedBytes: maximumRetainedBytes + 1})
	require.Equal(t, maximumRetainedBytes, clamped.config.RetainedBytes)
	clamped.Close()
	_, err := manager.Open(t.Context(), OpenRequest{})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = manager.Open(t.Context(), OpenRequest{ClientID: "c", SessionID: "s", Command: "fake", Cols: 1001})
	require.ErrorIs(t, err, ErrInvalidInput)
	metadata, err := manager.Open(t.Context(), OpenRequest{ClientID: "c", SessionID: "s", Command: "fake"})
	require.NoError(t, err)
	_, err = manager.Input(t.Context(), "c", "s", metadata.ID, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = manager.Resize("c", "s", metadata.ID, 0, 10)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestSessionCleanupWaitsForInFlightCallbacks(t *testing.T) {
	t.Parallel()

	factory := newFakeFactory()
	started := make(chan struct{})
	release := make(chan struct{})
	manager := New(Config{
		Factory: factory,
		OnOutput: func(string, string, uint64, []byte) {
			close(started)
			<-release
		},
	})
	t.Cleanup(manager.Close)
	metadata, err := manager.Open(t.Context(), OpenRequest{ClientID: "client", SessionID: "session", Command: "fake"})
	require.NoError(t, err)
	factory.latest().emit([]byte("output"))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("output callback did not start")
	}
	done := make(chan struct{})
	go func() {
		manager.CloseSession("session")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("session cleanup returned before callback completion")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session cleanup did not finish")
	}
	_, err = manager.Get("client", "session", metadata.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

type fakeFactory struct {
	mu        sync.Mutex
	processes []*fakeProcess
}

func newFakeFactory() *fakeFactory { return &fakeFactory{} }

func (f *fakeFactory) Start(context.Context, OpenRequest) (Process, error) {
	value := newFakeProcess()
	f.mu.Lock()
	f.processes = append(f.processes, value)
	f.mu.Unlock()
	return value, nil
}

func (f *fakeFactory) latest() *fakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.processes[len(f.processes)-1]
}

type fakeProcess struct {
	mu        sync.Mutex
	readCh    chan []byte
	exitCh    chan ProcessExit
	written   []byte
	size      [2]int
	closed    bool
	closeOnce sync.Once
}

func newFakeProcess() *fakeProcess {
	return &fakeProcess{readCh: make(chan []byte, 16), exitCh: make(chan ProcessExit, 1)}
}

func (p *fakeProcess) Read(value []byte) (int, error) {
	chunk, ok := <-p.readCh
	if !ok {
		return 0, io.EOF
	}
	return copy(value, chunk), nil
}

func (p *fakeProcess) Write(value []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	p.written = append(p.written, value...)
	return len(value), nil
}

func (p *fakeProcess) Resize(cols, rows int) error {
	p.mu.Lock()
	p.size = [2]int{cols, rows}
	p.mu.Unlock()
	return nil
}

func (p *fakeProcess) Kill(signal string) error {
	p.exit(ProcessExit{Code: 1, Signal: signal})
	return nil
}

func (p *fakeProcess) Wait(ctx context.Context) (ProcessExit, error) {
	select {
	case value := <-p.exitCh:
		return value, nil
	case <-ctx.Done():
		return ProcessExit{}, ctx.Err()
	}
}

func (p *fakeProcess) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.readCh)
	})
	return nil
}

func (p *fakeProcess) emit(value []byte) { p.readCh <- append([]byte(nil), value...) }
func (p *fakeProcess) exit(value ProcessExit) {
	select {
	case p.exitCh <- value:
	default:
	}
	_ = p.Close()
}
func (p *fakeProcess) writtenBytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.written...)
}
func (p *fakeProcess) dimensions() [2]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.size
}
func (p *fakeProcess) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

var _ Factory = (*fakeFactory)(nil)
var _ Process = (*fakeProcess)(nil)

func BenchmarkRetainedOutputRingAppend(b *testing.B) {
	ring := newByteRing(defaultRetainedBytes)
	chunk := make([]byte, 32*1024)
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for range b.N {
		ring.append(chunk)
	}
}
