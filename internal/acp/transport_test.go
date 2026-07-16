package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLineTransportFramingBoundsAndRecovery(t *testing.T) {
	t.Parallel()

	input := strings.NewReader(strings.Repeat("x", 17) + "\n{\"ok\":true}\r\n" + strings.Repeat("y", 16))
	transport := NewLineTransport(input, io.Discard, 16)

	_, err := transport.ReadFrame(t.Context())
	require.ErrorIs(t, err, ErrFrameTooLarge)
	frame, err := transport.ReadFrame(t.Context())
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":true}`, string(frame))
	frame, err = transport.ReadFrame(t.Context())
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("y", 16), string(frame), "an exact-limit EOF frame is valid")
	_, err = transport.ReadFrame(t.Context())
	require.ErrorIs(t, err, io.EOF)
}

func TestLineTransportWritesCompleteCleanFrames(t *testing.T) {
	t.Parallel()

	output := &shortWriter{limit: 2}
	transport := NewLineTransport(nil, output, 32)
	require.NoError(t, transport.WriteFrame(t.Context(), []byte(`{"ok":true}`)))
	require.Equal(t, "{\"ok\":true}\n", output.String())
	require.ErrorIs(t, transport.WriteFrame(t.Context(), []byte("one\ntwo")), ErrInvalidFrame)
	require.ErrorIs(t, transport.WriteFrame(t.Context(), bytes.Repeat([]byte("x"), 33)), ErrFrameTooLarge)
}

func TestLineTransportCancellationInterruptsBlockedWrite(t *testing.T) {
	t.Parallel()

	output := newBlockingWriteCloser()
	transport := NewLineTransport(nil, output, 32)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := transport.WriteFrame(ctx, []byte(`{"blocked":true}`))
	require.Error(t, err)
	require.Less(t, time.Since(started), 500*time.Millisecond)
	require.True(t, output.wasClosed())
}

func TestLineTransportCancellationInterruptsBlockedRead(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	transport := NewLineTransport(reader, io.Discard, 32)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := transport.ReadFrame(ctx)
	require.Error(t, err)
	require.Less(t, time.Since(started), 500*time.Millisecond)
}

func TestServerOversizedRecoveryAndProtocolOnlyOutput(t *testing.T) {
	t.Parallel()

	valid := `{"jsonrpc":"2.0","id":7,"method":"crush/test","params":{}}`
	input := strings.NewReader(strings.Repeat("x", maxMessageSize+1) + "\n" + valid + "\n")
	var output bytes.Buffer
	server := NewServerWithIO(nil, input, &output)
	server.SetExtensionRouter(extensionRouterFunc(func(context.Context, string, json.RawMessage) (any, *RPCError) {
		return map[string]bool{"ok": true}, nil
	}))
	require.NoError(t, server.Serve(t.Context()))

	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	require.Len(t, lines, 2, "transport output must contain protocol frames only")
	var oversized struct {
		JSONRPC string    `json:"jsonrpc"`
		ID      any       `json:"id"`
		Error   *RPCError `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &oversized))
	require.Nil(t, oversized.ID)
	require.Equal(t, CodeInvalidRequest, oversized.Error.Code)
	var response Response
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &response))
	require.NotNil(t, response.ID)
	require.Nil(t, response.Error)
	require.JSONEq(t, `{"ok":true}`, string(response.Result))
}

func TestServerReplacesOversizedOutboundResponseAndContinues(t *testing.T) {
	t.Parallel()

	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"crush/large","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"crush/small","params":{}}` + "\n",
	)
	var output bytes.Buffer
	server := NewServerWithIO(nil, input, &output)
	server.SetExtensionRouter(extensionRouterFunc(func(_ context.Context, method string, _ json.RawMessage) (any, *RPCError) {
		if method == "crush/large" {
			return strings.Repeat("x", maxMessageSize), nil
		}
		return map[string]bool{"ok": true}, nil
	}))
	require.NoError(t, server.Serve(t.Context()))

	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	responses := make(map[int64]Response, 2)
	for _, line := range lines {
		var response Response
		require.NoError(t, json.Unmarshal([]byte(line), &response))
		id, ok := response.ID.Int64()
		require.True(t, ok)
		responses[id] = response
	}
	require.Equal(t, "Response exceeds frame limit", responses[1].Error.Message)
	require.Nil(t, responses[2].Error)
	require.JSONEq(t, `{"ok":true}`, string(responses[2].Result))
}

func TestServerInvalidEncodingAndJSONReturnBoundedErrorsAndRecover(t *testing.T) {
	t.Parallel()

	inputBytes := append([]byte{0xff, '\n'}, []byte("not-json\n"+`{"jsonrpc":"2.0","id":3,"method":"crush/test","params":{}}`+"\n")...)
	var output bytes.Buffer
	server := NewServerWithIO(nil, bytes.NewReader(inputBytes), &output)
	server.SetExtensionRouter(extensionRouterFunc(func(context.Context, string, json.RawMessage) (any, *RPCError) {
		return map[string]bool{"ok": true}, nil
	}))
	require.NoError(t, server.Serve(t.Context()))

	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	require.Len(t, lines, 3)
	var response Response
	parseErrors := 0
	for _, line := range lines {
		if strings.Contains(line, `"id":null`) {
			require.Contains(t, line, `"code":-32700`)
			parseErrors++
			continue
		}
		require.NoError(t, json.Unmarshal([]byte(line), &response))
	}
	require.Equal(t, 2, parseErrors)
	id, ok := response.ID.Int64()
	require.True(t, ok)
	require.EqualValues(t, 3, id)
}

func TestServerPrioritizesSyncWritesAfterCurrentFrame(t *testing.T) {
	t.Parallel()

	transport := newContractTransport()
	transport.blockFirstWrite = true
	server := NewServerWithTransport(nil, transport)
	server.writeTimeout = time.Second
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(t.Context()) }()

	require.True(t, server.writeLineAsync([]byte("async-one")))
	require.Eventually(t, func() bool { return transport.firstWriteStarted() }, time.Second, time.Millisecond)
	require.True(t, server.writeLineAsync([]byte("async-two")))
	reliableDone := make(chan error, 1)
	go func() { reliableDone <- server.writeLineSync(t.Context(), []byte("reliable")) }()
	require.Eventually(t, func() bool { return len(server.writeSyncCh) == 1 }, time.Second, time.Millisecond)
	criticalDone := make(chan error, 1)
	go func() { criticalDone <- server.writeLineCritical(t.Context(), []byte("critical")) }()
	require.Eventually(t, func() bool { return len(server.writeCriticalCh) == 1 }, time.Second, time.Millisecond)
	transport.releaseFirst()
	require.NoError(t, <-criticalDone)
	require.NoError(t, <-reliableDone)
	require.Eventually(t, func() bool { return len(transport.writesSnapshot()) == 4 }, time.Second, time.Millisecond)
	require.Equal(t, []string{"async-one", "critical", "reliable", "async-two"}, transport.writesSnapshot())
	require.NoError(t, server.Close())
	require.NoError(t, <-serveDone)
}

func TestServerBlockedWriterHasByteAndTimeBounds(t *testing.T) {
	t.Parallel()

	transport := newContractTransport()
	transport.blockEveryWrite = true
	server := NewServerWithTransport(nil, transport)
	server.writeTimeout = 25 * time.Millisecond
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(t.Context()) }()

	payload := bytes.Repeat([]byte("x"), 64*1024)
	started := time.Now()
	accepted := 0
	for range 1000 {
		if server.writeLineAsync(payload) {
			accepted++
		}
	}
	require.Less(t, time.Since(started), 250*time.Millisecond, "producer-side notification enqueue must not block")
	require.LessOrEqual(t, server.asyncBytes.Load(), int64(maxQueuedWriteBytes))
	require.LessOrEqual(t, len(server.writeAsyncCh), writeAsyncBufSize)
	require.LessOrEqual(t, accepted, maxQueuedWriteBytes/len(payload)+1)
	require.Eventually(t, func() bool {
		select {
		case <-server.done:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	for range 100 {
		require.False(t, server.writeLineAsync(payload), "closed transport must reject new frames")
	}
	require.ErrorIs(t, server.writeLineSync(t.Context(), []byte("late")), ErrTransportDone)
	require.Error(t, <-serveDone)
	require.Zero(t, server.asyncBytes.Load())
	require.Empty(t, server.writeAsyncCh)
}

func TestServerResponseLifecycleObservesWriteFailureOnce(t *testing.T) {
	t.Parallel()

	transport := newContractTransport()
	transport.blockEveryWrite = true
	lifecycle := &trackingResponseLifecycle{called: make(chan error, 2)}
	server := NewServerWithTransport(nil, transport)
	server.writeTimeout = 20 * time.Millisecond
	server.SetExtensionRouter(extensionRouterFunc(func(context.Context, string, json.RawMessage) (any, *RPCError) {
		return lifecycle, nil
	}))
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(t.Context()) }()
	transport.feed([]byte(`{"jsonrpc":"2.0","id":1,"method":"crush/test","params":{}}`))

	select {
	case err := <-lifecycle.called:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("response lifecycle did not observe write failure")
	}
	select {
	case <-lifecycle.called:
		t.Fatal("response lifecycle was called more than once")
	case <-time.After(20 * time.Millisecond):
	}
	require.Equal(t, int32(1), lifecycle.calls.Load())
	require.Error(t, <-serveDone)
}

func TestServerDropsDuplicateResponseWithoutBlocking(t *testing.T) {
	t.Parallel()

	server := NewServerWithIO(nil, nil, nil)
	responses := make(chan *Response, 1)
	server.pending.Store(int64(1), responses)
	raw := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	server.dispatch(t.Context(), raw)
	done := make(chan struct{})
	go func() {
		server.dispatch(t.Context(), raw)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("duplicate response blocked dispatch")
	}
	require.Len(t, responses, 1)
}

func TestServerMissingPermissionResponseHonorsCallerDeadline(t *testing.T) {
	t.Parallel()

	transport := newContractTransport()
	server := NewServerWithTransport(nil, transport)
	serveCtx, cancelServe := context.WithCancel(t.Context())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(serveCtx) }()

	callCtx, cancelCall := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancelCall()
	_, err := server.Call(callCtx, "session/request_permission", map[string]string{"sessionId": "session"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Eventually(t, transport.firstWriteStarted, time.Second, time.Millisecond)

	cancelServe()
	require.NoError(t, server.Close())
	require.NoError(t, <-serveDone)
}

func TestServerEnforcesNestingAndMethodDeadline(t *testing.T) {
	t.Parallel()

	transport := newContractTransport()
	router := &deadlineRouter{called: make(chan struct{}, 1)}
	server := NewServerWithTransport(nil, transport)
	server.requestTimeout = func(string) time.Duration { return 20 * time.Millisecond }
	server.SetExtensionRouter(router)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(t.Context()) }()

	deep := `{"jsonrpc":"2.0","id":1,"method":"crush/test","params":` + strings.Repeat("[", maxJSONNesting+1) + "0" + strings.Repeat("]", maxJSONNesting+1) + "}"
	transport.feed([]byte(deep))
	transport.feed([]byte(`{"jsonrpc":"2.0","id":2,"method":"crush/test","params":{}}`))
	require.Eventually(t, func() bool { return router.calls() == 1 }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(transport.writesSnapshot()) == 2 }, time.Second, time.Millisecond)
	var response Response
	for _, raw := range transport.writesSnapshot() {
		var candidate Response
		require.NoError(t, json.Unmarshal([]byte(raw), &candidate))
		if candidate.ID != nil {
			response = candidate
		}
	}
	require.NotNil(t, response.Error)
	require.Equal(t, CodeInternalError, response.Error.Code)
	require.Equal(t, "Request deadline exceeded", response.Error.Message)
	require.NoError(t, server.Close())
	require.NoError(t, <-serveDone)
}

func TestServerBoundsInFlightRequestsWithoutStarvingResponses(t *testing.T) {
	t.Parallel()

	transport := newContractTransport()
	transport.readFrames = make(chan []byte, 64)
	router := &capacityRouter{release: make(chan struct{})}
	server := NewServerWithTransport(nil, transport)
	server.SetExtensionRouter(router)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(t.Context()) }()

	large := strings.Repeat("x", 1024*1024)
	for id := 1; id <= 24; id++ {
		frame, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": "crush/capacity",
			"params": map[string]string{"value": large},
		})
		require.NoError(t, err)
		transport.feed(frame)
	}
	require.Eventually(t, func() bool {
		return router.calls() > 0 && len(transport.writesSnapshot()) > 0
	}, 2*time.Second, time.Millisecond)
	require.LessOrEqual(t, server.dispatchBytes.Load(), int64(maxInFlightRequestBytes))
	require.LessOrEqual(t, router.calls(), maxConcurrentRequests)

	responseChannel := make(chan *Response, 1)
	server.pending.Store(int64(99), responseChannel)
	transport.feed([]byte(`{"jsonrpc":"2.0","id":99,"result":{"ok":true}}`))
	select {
	case response := <-responseChannel:
		require.JSONEq(t, `{"ok":true}`, string(response.Result))
	case <-time.After(time.Second):
		t.Fatal("outgoing-call response was starved by request capacity")
	}

	close(router.release)
	require.Eventually(t, func() bool { return server.dispatchBytes.Load() == 0 }, 2*time.Second, time.Millisecond)
	require.NoError(t, server.Close())
	require.NoError(t, <-serveDone)
}

type extensionRouterFunc func(context.Context, string, json.RawMessage) (any, *RPCError)

func (f extensionRouterFunc) HandleExtension(ctx context.Context, method string, params json.RawMessage) (any, *RPCError) {
	return f(ctx, method, params)
}

type trackingResponseLifecycle struct {
	calls  atomic.Int32
	called chan error
}

func (*trackingResponseLifecycle) ResponseResult() any { return map[string]bool{"ok": true} }

func (l *trackingResponseLifecycle) AfterResponse(_ context.Context, err error) {
	l.calls.Add(1)
	l.called <- err
}

type shortWriter struct {
	bytes.Buffer
	limit int
}

func (w *shortWriter) Write(value []byte) (int, error) {
	return w.Buffer.Write(value[:min(len(value), w.limit)])
}

type blockingWriteCloser struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{closed: make(chan struct{})}
}

func (w *blockingWriteCloser) Write([]byte) (int, error) {
	<-w.closed
	return 0, io.ErrClosedPipe
}

func (w *blockingWriteCloser) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

func (w *blockingWriteCloser) wasClosed() bool {
	select {
	case <-w.closed:
		return true
	default:
		return false
	}
}

type contractTransport struct {
	readFrames chan []byte
	closed     chan struct{}
	closeOnce  sync.Once

	mu               sync.Mutex
	writes           []string
	writeCalls       int
	blockFirstWrite  bool
	blockEveryWrite  bool
	firstStarted     chan struct{}
	firstStartedOnce sync.Once
	firstRelease     chan struct{}
	firstReleaseOnce sync.Once
}

func newContractTransport() *contractTransport {
	return &contractTransport{
		readFrames: make(chan []byte, 8), closed: make(chan struct{}),
		firstStarted: make(chan struct{}), firstRelease: make(chan struct{}),
	}
}

func (*contractTransport) Name() string { return "contract" }

func (t *contractTransport) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case frame := <-t.readFrames:
		return append([]byte(nil), frame...), nil
	case <-t.closed:
		return nil, ErrTransportDone
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *contractTransport) WriteFrame(ctx context.Context, frame []byte) error {
	t.mu.Lock()
	t.writeCalls++
	call := t.writeCalls
	block := t.blockEveryWrite || (t.blockFirstWrite && call == 1)
	t.mu.Unlock()
	if call == 1 {
		t.firstStartedOnce.Do(func() { close(t.firstStarted) })
	}
	if block {
		select {
		case <-t.firstRelease:
		case <-t.closed:
			return ErrTransportDone
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.mu.Lock()
	t.writes = append(t.writes, string(frame))
	t.mu.Unlock()
	return nil
}

func (t *contractTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

func (t *contractTransport) feed(frame []byte) {
	t.readFrames <- append([]byte(nil), frame...)
}

func (t *contractTransport) firstWriteStarted() bool {
	select {
	case <-t.firstStarted:
		return true
	default:
		return false
	}
}

func (t *contractTransport) releaseFirst() {
	t.firstReleaseOnce.Do(func() { close(t.firstRelease) })
}

func (t *contractTransport) writesSnapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.writes...)
}

type deadlineRouter struct {
	mu     sync.Mutex
	count  int
	called chan struct{}
}

type capacityRouter struct {
	mu      sync.Mutex
	count   int
	release chan struct{}
}

func (r *capacityRouter) HandleExtension(ctx context.Context, _ string, _ json.RawMessage) (any, *RPCError) {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
	select {
	case <-r.release:
		return map[string]bool{"ok": true}, nil
	case <-ctx.Done():
		return nil, &RPCError{Code: CodeInternalError, Message: "cancelled"}
	}
}

func (r *capacityRouter) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

func (r *deadlineRouter) HandleExtension(ctx context.Context, _ string, _ json.RawMessage) (any, *RPCError) {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
	select {
	case r.called <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, nil
}

func (r *deadlineRouter) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

func TestValidateJSONNestingIgnoresQuotedDelimiters(t *testing.T) {
	t.Parallel()
	require.NoError(t, validateJSONNesting([]byte(`{"value":"[[[{{{"}`), 2))
	require.Error(t, validateJSONNesting([]byte(`[[[0]]]`), 2))
}
