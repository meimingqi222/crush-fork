package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/crush/internal/guimetrics"
)

const (
	// writeSyncBufSize is the buffer size for high-priority synchronous writes
	// (responses and outgoing calls). Keep small since these are infrequent.
	writeSyncBufSize     = 32
	writeCriticalBufSize = 32
	// writeAsyncBufSize is the buffer size for low-priority async writes
	// (notifications). Large enough to absorb bursts of streaming deltas.
	writeAsyncBufSize = 256
	// maxMessageSize is the largest single JSON-RPC message the server will
	// accept. Oversized messages are discarded and the server continues.
	maxMessageSize = 4 * 1024 * 1024
	// maxQueuedWriteBytes bounds retained frames independently of queue count.
	// One in-progress frame may temporarily sit above this queued-only budget.
	maxQueuedWriteBytes     = 8 * 1024 * 1024
	maxConcurrentRequests   = 128
	maxInFlightRequestBytes = 16 * 1024 * 1024
	defaultWriteTimeout     = 10 * time.Second
	defaultRequestTimeout   = 30 * time.Second
	longRequestTimeout      = 30 * time.Minute
	maxJSONNesting          = 64
)

// writeRequest is a single serialized transport write.
type writeRequest struct {
	data       []byte
	ack        chan error // nil = async (fire-and-forget)
	enqueuedAt time.Time
	ctx        context.Context
	barrier    bool
}

// Server is the transport-independent ACP JSON-RPC 2.0 connection runtime.
type Server struct {
	handler         *Handler
	extensionRouter ExtensionRouter

	transport Transport

	// writeCriticalCh carries responses and outgoing calls. These must not wait
	// behind a burst of reliable GUI or standard ACP notifications.
	writeCriticalCh chan writeRequest
	// writeSyncCh carries reliable notifications. The writer goroutine drains
	// this channel before best-effort writeAsyncCh.
	writeSyncCh chan writeRequest
	// writeAsyncCh carries low-priority, async writes (notifications).
	// Notifications are dropped when this channel is full to prevent the
	// event-processing loop from blocking and causing deadlocks with
	// permission-request writes.
	writeAsyncCh  chan writeRequest
	criticalBytes atomic.Int64
	syncBytes     atomic.Int64
	asyncBytes    atomic.Int64
	criticalSpace chan struct{}
	syncSpace     chan struct{}
	asyncSpace    chan struct{}
	writerDone    chan struct{}
	done          chan struct{}
	closeOnce     sync.Once
	enqueueMu     sync.RWMutex
	accepting     bool
	serveStarted  atomic.Bool
	writerErrMu   sync.Mutex
	writerErr     error
	dispatchWG    sync.WaitGroup
	dispatchSlots chan struct{}
	dispatchBytes atomic.Int64

	writeTimeout   time.Duration
	requestTimeout func(string) time.Duration

	nextID  atomic.Int64
	pending sync.Map // id -> chan *Response
}

// ExtensionRouter handles private method namespaces independently from the
// standard ACP Handler.
type ExtensionRouter interface {
	HandleExtension(ctx context.Context, method string, params json.RawMessage) (any, *RPCError)
}

// ResponseLifecycle lets a handler delay side effects until its JSON-RPC
// response has been written. AfterResponse is called exactly once, including
// notifications and write failures.
type ResponseLifecycle interface {
	ResponseResult() any
	AfterResponse(ctx context.Context, writeErr error)
}

// NewServer creates a new ACP server using stdin/stdout.
func NewServer(handler *Handler) *Server {
	return NewServerWithTransport(handler, NewLineTransport(os.Stdin, os.Stdout, maxMessageSize))
}

// NewServerWithIO creates a new ACP server with custom IO streams (for testing).
func NewServerWithIO(handler *Handler, in io.Reader, out io.Writer) *Server {
	return NewServerWithTransport(handler, NewLineTransport(in, out, maxMessageSize))
}

// NewServerWithTransport creates an ACP server over a framed connection. The
// same JSON-RPC contract can therefore be reused by future pipe or socket
// transports without moving session semantics out of Server.
func NewServerWithTransport(handler *Handler, transport Transport) *Server {
	if transport == nil {
		transport = NewLineTransport(nil, nil, maxMessageSize)
	}
	return &Server{
		handler: handler, transport: transport,
		writeCriticalCh: make(chan writeRequest, writeCriticalBufSize),
		writeSyncCh:     make(chan writeRequest, writeSyncBufSize),
		writeAsyncCh:    make(chan writeRequest, writeAsyncBufSize),
		criticalSpace:   make(chan struct{}, 1), syncSpace: make(chan struct{}, 1),
		asyncSpace: make(chan struct{}, 1),
		writerDone: make(chan struct{}), done: make(chan struct{}),
		accepting:     true,
		dispatchSlots: make(chan struct{}, maxConcurrentRequests),
		writeTimeout:  defaultWriteTimeout, requestTimeout: requestDeadline,
	}
}

// SetExtensionRouter installs the connection-scoped private protocol router.
// It must be called before Serve starts.
func (s *Server) SetExtensionRouter(router ExtensionRouter) {
	s.extensionRouter = router
}

// runWriter is a dedicated goroutine that serializes all writes to out.
// It prioritizes writeSyncCh (responses, outgoing calls) over writeAsyncCh
// (notifications) to prevent slow notification writes from delaying critical
// messages such as permission requests.
func (s *Server) runWriter(ctx context.Context) {
	defer func() {
		s.discardQueuedWrites(ErrTransportDone)
		close(s.writerDone)
	}()
	recorder := guimetrics.FromContext(ctx)
	transportName := "other"
	if s.transport != nil && s.transport.Name() != "" {
		transportName = normalizeTransportName(s.transport.Name())
	}
	for {
		req, ok := s.nextWrite(ctx)
		if !ok {
			return
		}
		recorder.SetGauge(guimetrics.GUIEventQueueDepth, int64(len(s.writeCriticalCh)+len(s.writeSyncCh)+len(s.writeAsyncCh)), guimetrics.Labels{Transport: transportName})
		if err := req.ctx.Err(); err != nil {
			s.ackWrite(req, err)
			continue
		}
		if req.barrier {
			s.ackWrite(req, nil)
			continue
		}
		writeCtx, cancel := context.WithTimeout(req.ctx, s.writeTimeout)
		err := s.transport.WriteFrame(writeCtx, req.data)
		cancel()
		if !req.enqueuedAt.IsZero() {
			outcome := "success"
			if err != nil {
				outcome = "error"
			}
			recorder.ObserveDuration(guimetrics.StreamEventToWriteDuration, time.Since(req.enqueuedAt), guimetrics.Labels{
				Outcome: outcome, Transport: transportName,
			})
		}
		s.ackWrite(req, err)
		if err != nil {
			s.setWriterError(err)
			s.closeConnection()
			return
		}
	}
}

func (s *Server) discardQueuedWrites(err error) {
	for {
		select {
		case req := <-s.writeCriticalCh:
			s.releaseQueuedBytes(&s.criticalBytes, len(req.data), s.criticalSpace)
			s.ackWrite(req, err)
		default:
			goto reliable
		}
	}

reliable:
	for {
		select {
		case req := <-s.writeSyncCh:
			s.releaseQueuedBytes(&s.syncBytes, len(req.data), s.syncSpace)
			s.ackWrite(req, err)
		default:
			goto async
		}
	}

async:
	for {
		select {
		case req := <-s.writeAsyncCh:
			s.releaseQueuedBytes(&s.asyncBytes, len(req.data), s.asyncSpace)
		default:
			return
		}
	}
}

func (s *Server) nextWrite(ctx context.Context) (writeRequest, bool) {
	select {
	case req := <-s.writeCriticalCh:
		s.releaseQueuedBytes(&s.criticalBytes, len(req.data), s.criticalSpace)
		return req, true
	case <-ctx.Done():
		return writeRequest{}, false
	case <-s.done:
		return writeRequest{}, false
	default:
	}
	select {
	case req := <-s.writeSyncCh:
		s.releaseQueuedBytes(&s.syncBytes, len(req.data), s.syncSpace)
		return req, true
	case <-ctx.Done():
		return writeRequest{}, false
	case <-s.done:
		return writeRequest{}, false
	default:
	}
	select {
	case req := <-s.writeCriticalCh:
		s.releaseQueuedBytes(&s.criticalBytes, len(req.data), s.criticalSpace)
		return req, true
	case req := <-s.writeSyncCh:
		s.releaseQueuedBytes(&s.syncBytes, len(req.data), s.syncSpace)
		return req, true
	case req := <-s.writeAsyncCh:
		s.releaseQueuedBytes(&s.asyncBytes, len(req.data), s.asyncSpace)
		return req, true
	case <-ctx.Done():
		return writeRequest{}, false
	case <-s.done:
		return writeRequest{}, false
	}
}

func (*Server) ackWrite(req writeRequest, err error) {
	if req.ack == nil {
		return
	}
	select {
	case req.ack <- err:
	default:
	}
}

func (s *Server) writeLineCritical(ctx context.Context, data []byte) error {
	if len(data) > maxMessageSize {
		return ErrFrameTooLarge
	}
	ack := make(chan error, 1)
	for {
		if reserveQueuedBytes(&s.criticalBytes, len(data), maxQueuedWriteBytes) {
			s.enqueueMu.RLock()
			if !s.accepting {
				s.enqueueMu.RUnlock()
				s.releaseQueuedBytes(&s.criticalBytes, len(data), s.criticalSpace)
				return ErrTransportDone
			}
			req := writeRequest{data: data, ack: ack, enqueuedAt: time.Now(), ctx: ctx}
			select {
			case s.writeCriticalCh <- req:
				s.enqueueMu.RUnlock()
				goto wait
			default:
				s.enqueueMu.RUnlock()
				s.releaseQueuedBytes(&s.criticalBytes, len(data), s.criticalSpace)
			}
		}
		select {
		case <-s.criticalSpace:
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return ErrTransportDone
		}
	}

wait:
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrTransportDone
	}
}

// writeLineSync enqueues data for a synchronous, high-priority write and
// blocks until the write completes or ctx is cancelled.
func (s *Server) writeLineSync(ctx context.Context, data []byte) error {
	if len(data) > maxMessageSize {
		return ErrFrameTooLarge
	}
	ack := make(chan error, 1)
	for {
		if reserveQueuedBytes(&s.syncBytes, len(data), maxQueuedWriteBytes) {
			s.enqueueMu.RLock()
			if !s.accepting {
				s.enqueueMu.RUnlock()
				s.releaseQueuedBytes(&s.syncBytes, len(data), s.syncSpace)
				return ErrTransportDone
			}
			req := writeRequest{data: data, ack: ack, enqueuedAt: time.Now(), ctx: ctx}
			select {
			case s.writeSyncCh <- req:
				s.enqueueMu.RUnlock()
				goto wait
			default:
				s.enqueueMu.RUnlock()
				s.releaseQueuedBytes(&s.syncBytes, len(data), s.syncSpace)
			}
		}
		select {
		case <-s.syncSpace:
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return ErrTransportDone
		}
	}

wait:
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrTransportDone
	}
}

func (s *Server) tryWriteLinePriority(ctx context.Context, data []byte) bool {
	if len(data) > maxMessageSize {
		return false
	}
	s.enqueueMu.RLock()
	defer s.enqueueMu.RUnlock()
	if !s.accepting || !reserveQueuedBytes(&s.criticalBytes, len(data), maxQueuedWriteBytes) {
		return false
	}
	req := writeRequest{data: data, enqueuedAt: time.Now(), ctx: ctx}
	select {
	case s.writeCriticalCh <- req:
		return true
	default:
		s.releaseQueuedBytes(&s.criticalBytes, len(data), s.criticalSpace)
		return false
	}
}

func (s *Server) flushPriority(ctx context.Context) error {
	ack := make(chan error, 1)
	for {
		s.enqueueMu.RLock()
		if !s.accepting {
			s.enqueueMu.RUnlock()
			return ErrTransportDone
		}
		select {
		case s.writeCriticalCh <- writeRequest{ack: ack, ctx: ctx, barrier: true}:
			s.enqueueMu.RUnlock()
			select {
			case err := <-ack:
				return err
			case <-ctx.Done():
				return ctx.Err()
			case <-s.done:
				return ErrTransportDone
			}
		default:
			s.enqueueMu.RUnlock()
		}
		select {
		case <-s.criticalSpace:
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return ErrTransportDone
		}
	}
}

// writeLineAsync enqueues data for an async, low-priority write and returns
// immediately. Returns false if the async buffer is full and the write is dropped.
func (s *Server) writeLineAsync(data []byte) bool {
	if len(data) > maxMessageSize {
		return false
	}
	s.enqueueMu.RLock()
	defer s.enqueueMu.RUnlock()
	if !s.accepting || !reserveQueuedBytes(&s.asyncBytes, len(data), maxQueuedWriteBytes) {
		return false
	}
	req := writeRequest{data: data, enqueuedAt: time.Now(), ctx: context.Background()}
	select {
	case s.writeAsyncCh <- req:
		return true
	default:
		s.releaseQueuedBytes(&s.asyncBytes, len(data), s.asyncSpace)
		return false
	}
}

// Serve reads JSON-RPC messages from stdin and dispatches them until ctx is
// cancelled or the input stream is closed.
func (s *Server) Serve(ctx context.Context) error {
	if !s.serveStarted.CompareAndSwap(false, true) {
		return errors.New("acp: server already served")
	}
	serveCtx, cancel := context.WithCancel(withTransportName(ctx, s.transport.Name()))
	go s.runWriter(serveCtx)
	gracefulEOF := false
	defer func() {
		if gracefulEOF {
			s.dispatchWG.Wait()
			cancel()
			s.closeConnection()
		} else {
			cancel()
			s.closeConnection()
			s.dispatchWG.Wait()
		}
		<-s.writerDone
	}()

	for {
		if err := serveCtx.Err(); err != nil {
			// Context cancellation (e.g. SIGINT via NotifyContext) is a
			// clean shutdown, not an error the CLI should surface.
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}

		frame, err := s.transport.ReadFrame(serveCtx)
		if err != nil && errors.Is(serveCtx.Err(), context.Canceled) {
			return nil
		}
		if errors.Is(err, ErrFrameTooLarge) {
			slog.Warn("ACP: oversized message discarded", "max_size", maxMessageSize)
			s.writeNullIDError(serveCtx, CodeInvalidRequest, "Request frame exceeds size limit")
			continue
		}
		if errors.Is(err, ErrInvalidEncoding) {
			s.writeNullIDError(serveCtx, CodeParseError, "Request frame is not valid UTF-8")
			continue
		}
		if errors.Is(err, io.EOF) {
			gracefulEOF = true
			s.dispatchWG.Wait()
			if flushErr := s.flushPriority(serveCtx); flushErr != nil {
				return fmt.Errorf("acp: flush error: %w", flushErr)
			}
			if writerErr := s.getWriterError(); writerErr != nil {
				return fmt.Errorf("acp: write error: %w", writerErr)
			}
			return nil
		}
		if errors.Is(err, ErrTransportDone) {
			if contextErr := serveCtx.Err(); contextErr != nil {
				if errors.Is(contextErr, context.Canceled) {
					return nil
				}
				return contextErr
			}
			if writerErr := s.getWriterError(); writerErr != nil {
				return fmt.Errorf("acp: write error: %w", writerErr)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("acp: read error: %w", err)
		}
		raw := json.RawMessage(frame)
		if err := validateJSONNesting(raw, maxJSONNesting); err != nil {
			slog.Warn("ACP: invalid JSON nesting", "error", err)
			s.writeNullIDError(serveCtx, CodeInvalidRequest, "Request JSON exceeds nesting limit")
			continue
		}
		if isResponseFrame(raw) {
			s.dispatch(serveCtx, raw)
			continue
		}
		if !s.startDispatch(serveCtx, raw) {
			s.rejectOverloaded(serveCtx, raw)
		}
	}
}

func (s *Server) startDispatch(ctx context.Context, raw json.RawMessage) bool {
	if !reserveQueuedBytes(&s.dispatchBytes, len(raw), maxInFlightRequestBytes) {
		return false
	}
	select {
	case s.dispatchSlots <- struct{}{}:
		s.dispatchWG.Add(1)
		go func() {
			defer s.dispatchWG.Done()
			defer func() { <-s.dispatchSlots }()
			defer s.dispatchBytes.Add(-int64(len(raw)))
			s.dispatch(ctx, raw)
		}()
		return true
	default:
		s.dispatchBytes.Add(-int64(len(raw)))
		return false
	}
}

func (s *Server) rejectOverloaded(ctx context.Context, raw json.RawMessage) {
	var peek struct {
		ID *ID `json:"id"`
	}
	if json.Unmarshal(raw, &peek) != nil || peek.ID == nil {
		return
	}
	encoded, err := json.Marshal(Response{
		JSONRPC: "2.0", ID: peek.ID,
		Error: &RPCError{Code: CodeInternalError, Message: "ACP server request capacity exhausted"},
	})
	if err != nil || !s.tryWriteLinePriority(ctx, encoded) {
		s.closeConnection()
	}
}

func isResponseFrame(raw json.RawMessage) bool {
	var peek struct {
		ID     *ID    `json:"id"`
		Method string `json:"method"`
	}
	return json.Unmarshal(raw, &peek) == nil && peek.ID != nil && peek.Method == ""
}

func (s *Server) writeNullIDError(ctx context.Context, code int, message string) {
	encoded, err := json.Marshal(struct {
		JSONRPC string    `json:"jsonrpc"`
		ID      any       `json:"id"`
		Error   *RPCError `json:"error"`
	}{JSONRPC: "2.0", ID: nil, Error: &RPCError{Code: code, Message: message}})
	if err != nil || !s.tryWriteLinePriority(ctx, encoded) {
		s.closeConnection()
	}
}

// Close terminates the connection and interrupts cancellable transport I/O.
// It is idempotent and safe to call from connection-owner cleanup.
func (s *Server) Close() error {
	return s.closeConnection()
}

func (s *Server) closeConnection() error {
	var err error
	s.closeOnce.Do(func() {
		s.enqueueMu.Lock()
		s.accepting = false
		close(s.done)
		s.enqueueMu.Unlock()
		if s.transport != nil {
			err = s.transport.Close()
		}
	})
	return err
}

func (s *Server) setWriterError(err error) {
	s.writerErrMu.Lock()
	if s.writerErr == nil {
		s.writerErr = err
	}
	s.writerErrMu.Unlock()
}

func (s *Server) getWriterError() error {
	s.writerErrMu.Lock()
	defer s.writerErrMu.Unlock()
	return s.writerErr
}

func reserveQueuedBytes(counter *atomic.Int64, size, limit int) bool {
	if size < 0 || size > limit {
		return false
	}
	for {
		current := counter.Load()
		if current > int64(limit-size) {
			return false
		}
		if counter.CompareAndSwap(current, current+int64(size)) {
			return true
		}
	}
}

func (*Server) releaseQueuedBytes(counter *atomic.Int64, size int, signal chan<- struct{}) {
	counter.Add(-int64(size))
	select {
	case signal <- struct{}{}:
	default:
	}
}

func requestDeadline(method string) time.Duration {
	switch method {
	case "session/prompt":
		return longRequestTimeout
	case "session/request_permission":
		return 5 * time.Minute
	case "crush/turn/wait":
		return 5*time.Minute + 5*time.Second
	default:
		return defaultRequestTimeout
	}
}

func validateJSONNesting(raw []byte, limit int) error {
	depth := 0
	inString := false
	escaped := false
	for _, value := range raw {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch value {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch value {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > limit {
				return errors.New("json nesting limit exceeded")
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return errors.New("json nesting is unbalanced")
			}
		}
	}
	return nil
}

// dispatch determines the message kind and handles it.
func (s *Server) dispatch(ctx context.Context, raw json.RawMessage) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("ACP: panic in dispatch", "panic", r)
			// Best-effort: try to parse the request ID so we can send an
			// error response back to the client instead of silently
			// dropping the request.
			var peek struct {
				ID *ID `json:"id"`
			}
			if json.Unmarshal(raw, &peek) == nil && peek.ID != nil {
				s.writeResponse(ctx, &Response{
					JSONRPC: "2.0",
					ID:      peek.ID,
					Error:   &RPCError{Code: CodeInternalError, Message: "Internal server error"},
				})
			}
		}
	}()
	if err := validateJSONNesting(raw, maxJSONNesting); err != nil {
		slog.Warn("ACP: invalid JSON nesting", "error", err)
		s.writeNullIDError(ctx, CodeInvalidRequest, "Request JSON exceeds nesting limit")
		return
	}

	// Peek at the message to determine type.
	var peek struct {
		ID     *ID             `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		slog.Warn("ACP: failed to parse message", "err", err)
		s.writeNullIDError(ctx, CodeParseError, "Invalid JSON-RPC message")
		return
	}

	// If it has Result or Error and an ID, it's a response to our outgoing call.
	if peek.ID != nil && peek.Method == "" {
		var resp Response
		if err := json.Unmarshal(raw, &resp); err != nil {
			slog.Warn("ACP: failed to parse response", "err", err)
			return
		}
		var pendingID int64
		if resp.ID != nil {
			if num, ok := resp.ID.Int64(); ok {
				pendingID = num
			} else if resp.ID.str != nil {
				if num, err := strconv.ParseInt(*resp.ID.str, 10, 64); err == nil {
					pendingID = num
				}
			}
		}
		if ch, ok := s.pending.Load(pendingID); ok {
			select {
			case ch.(chan *Response) <- &resp:
			default:
				slog.Warn("ACP: duplicate response dropped for pending call", "id", pendingID)
			}
		}
		return
	}

	// Otherwise it's a request or notification from the client.
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		slog.Warn("ACP: failed to parse request", "err", err)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.requestTimeout(req.Method))
	defer cancel()

	var result any
	var rpcErr *RPCError
	if strings.HasPrefix(req.Method, "crush/") {
		if s.extensionRouter == nil {
			rpcErr = &RPCError{Code: CodeMethodNotFound, Message: fmt.Sprintf("method not found: %s", req.Method)}
		} else {
			result, rpcErr = s.extensionRouter.HandleExtension(requestCtx, req.Method, req.Params)
		}
	} else {
		result, rpcErr = s.handler.Handle(requestCtx, &req)
	}
	if errors.Is(requestCtx.Err(), context.DeadlineExceeded) && rpcErr == nil {
		result = nil
		rpcErr = &RPCError{Code: CodeInternalError, Message: "Request deadline exceeded"}
	}

	var lifecycle ResponseLifecycle
	if value, ok := result.(ResponseLifecycle); ok {
		lifecycle = value
		result = value.ResponseResult()
	}

	// Notifications have no ID and expect no response.
	if req.ID == nil {
		if lifecycle != nil {
			lifecycle.AfterResponse(ctx, errors.New("json-rpc notification has no response lifecycle"))
		}
		return
	}

	var resp Response
	resp.JSONRPC = "2.0"
	resp.ID = req.ID
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		encoded, err := json.Marshal(result)
		if err != nil {
			resp.Error = &RPCError{Code: CodeInternalError, Message: "Failed to encode response"}
		} else {
			resp.Result = encoded
		}
	}
	writeErr := s.writeResponse(ctx, &resp)
	if lifecycle != nil {
		lifecycle.AfterResponse(ctx, writeErr)
	}
}

// writeResponse encodes and writes a response synchronously via the writer goroutine.
func (s *Server) writeResponse(ctx context.Context, resp *Response) error {
	b, err := json.Marshal(resp)
	if err != nil {
		slog.Error("ACP: failed to marshal response", "err", err)
		return err
	}
	if len(b) > maxMessageSize {
		b, err = json.Marshal(Response{
			JSONRPC: "2.0", ID: resp.ID,
			Error: &RPCError{Code: CodeInternalError, Message: "Response exceeds frame limit"},
		})
		if err != nil {
			return err
		}
	}
	if err := s.writeLineCritical(ctx, b); err != nil {
		slog.Error("ACP: failed to write response", "err", err)
		return err
	}
	return nil
}

// Notify sends a notification (no id, no response expected) to the client.
// The write is asynchronous and non-blocking: if the output buffer is full
// the notification is dropped. This prevents slow client reads from stalling
// the agent event loop or blocking permission-request writes.
func (s *Server) Notify(_ context.Context, method string, params any) {
	b, err := json.Marshal(params)
	if err != nil {
		slog.Error("ACP: failed to marshal notification params", "method", method, "err", err)
		return
	}
	msg := Request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  b,
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		slog.Error("ACP: failed to marshal notification", "method", method, "err", err)
		return
	}
	if !s.writeLineAsync(raw) {
		slog.Warn("ACP: notification dropped, output buffer full", "method", method)
	}
}

// NotifySync sends a notification and blocks until it is written.
func (s *Server) NotifySync(ctx context.Context, method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("acp: marshal notification params: %w", err)
	}
	msg := Request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  b,
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("acp: marshal notification: %w", err)
	}
	if err := s.writeLineSync(ctx, raw); err != nil {
		return fmt.Errorf("acp: write notification: %w", err)
	}
	return nil
}

// Call sends a request to the client and waits for its response.
// Returns the raw result JSON or an error.
func (s *Server) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	callCtx, cancel := context.WithTimeout(ctx, s.requestTimeout(method))
	defer cancel()
	id := s.nextID.Add(1)

	b, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("acp: marshal params: %w", err)
	}
	req := Request{
		JSONRPC: "2.0",
		ID:      NewIDFromInt(id),
		Method:  method,
		Params:  b,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("acp: marshal request: %w", err)
	}

	ch := make(chan *Response, 1)
	s.pending.Store(id, ch)
	defer s.pending.Delete(id)

	if err := s.writeLineCritical(callCtx, raw); err != nil {
		return nil, fmt.Errorf("acp: write request: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-callCtx.Done():
		return nil, callCtx.Err()
	case <-s.done:
		return nil, ErrTransportDone
	}
}
