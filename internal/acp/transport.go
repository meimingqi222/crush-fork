package acp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"
)

var (
	ErrFrameTooLarge   = errors.New("acp transport frame exceeds limit")
	ErrInvalidFrame    = errors.New("acp transport frame contains a line delimiter")
	ErrInvalidEncoding = errors.New("acp transport frame is not valid UTF-8")
	ErrTransportDone   = errors.New("acp transport is closed")
)

type transportNameContextKey struct{}

// TransportName returns the bounded metric label installed by Server. Direct
// Handler calls retain the stdio compatibility default.
func TransportName(ctx context.Context) string {
	if name, ok := ctx.Value(transportNameContextKey{}).(string); ok && name != "" {
		return normalizeTransportName(name)
	}
	return "stdio"
}

func withTransportName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, transportNameContextKey{}, normalizeTransportName(name))
}

func normalizeTransportName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "stdio":
		return "stdio"
	case "pipe", "named_pipe":
		return "pipe"
	case "socket", "unix_socket":
		return "socket"
	case "websocket":
		return "websocket"
	default:
		return "other"
	}
}

// Transport is the connection boundary used by the ACP JSON-RPC server.
// Implementations own framing, bounded reads, complete writes, peer close
// detection and cancellation of blocked I/O.
type Transport interface {
	Name() string
	ReadFrame(context.Context) ([]byte, error)
	WriteFrame(context.Context, []byte) error
	Close() error
}

// LineTransport implements newline-delimited JSON framing for stdio and pipe
// connections. Context cancellation interrupts blocked I/O when the underlying
// reader or writer implements io.Closer; production stdio satisfies this
// contract.
type LineTransport struct {
	reader   *bufio.Reader
	input    io.Reader
	output   io.Writer
	maxFrame int

	writeGate chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func NewLineTransport(input io.Reader, output io.Writer, maxFrame int) *LineTransport {
	if maxFrame <= 0 {
		maxFrame = maxMessageSize
	}
	transport := &LineTransport{
		input: input, output: output, maxFrame: maxFrame,
		writeGate: make(chan struct{}, 1), closed: make(chan struct{}),
	}
	transport.writeGate <- struct{}{}
	if input != nil {
		// One extra byte lets a frame exactly at the limit include its newline
		// without being misclassified as oversized.
		transport.reader = bufio.NewReaderSize(input, maxFrame+1)
	}
	return transport
}

func (*LineTransport) Name() string { return "stdio" }

func (t *LineTransport) ReadFrame(ctx context.Context) ([]byte, error) {
	if t.reader == nil {
		return nil, io.EOF
	}
	if err := t.operationReady(ctx); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = t.Close() })
	defer stop()

	line, err := t.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		if drainErr := t.drainFrame(); drainErr != nil {
			return nil, fmt.Errorf("acp transport drain frame: %w", drainErr)
		}
		return nil, ErrFrameTooLarge
	}
	if errors.Is(err, io.EOF) {
		if len(line) == 0 {
			return nil, io.EOF
		}
	} else if err != nil {
		select {
		case <-t.closed:
			return nil, ErrTransportDone
		default:
		}
		return nil, err
	}

	line = trimLineEnding(line)
	if len(line) > t.maxFrame {
		return nil, ErrFrameTooLarge
	}
	if !utf8.Valid(line) {
		return nil, ErrInvalidEncoding
	}
	frame := make([]byte, len(line))
	copy(frame, line)
	return frame, nil
}

func (t *LineTransport) WriteFrame(ctx context.Context, frame []byte) error {
	if len(frame) > t.maxFrame {
		return ErrFrameTooLarge
	}
	if bytes.ContainsAny(frame, "\r\n") {
		return ErrInvalidFrame
	}
	if err := t.operationReady(ctx); err != nil {
		return err
	}
	select {
	case <-t.writeGate:
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closed:
		return ErrTransportDone
	}
	defer func() { t.writeGate <- struct{}{} }()
	if err := t.operationReady(ctx); err != nil {
		return err
	}
	if t.output == nil {
		return io.ErrClosedPipe
	}

	stop := context.AfterFunc(ctx, func() { _ = t.Close() })
	defer stop()
	payload := make([]byte, len(frame)+1)
	copy(payload, frame)
	payload[len(frame)] = '\n'
	for len(payload) > 0 {
		written, err := t.output.Write(payload)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrNoProgress
		}
		payload = payload[written:]
	}
	return nil
}

func (t *LineTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		var errs []error
		if closer, ok := t.input.(io.Closer); ok {
			errs = append(errs, closer.Close())
		}
		if closer, ok := t.output.(io.Closer); ok {
			errs = append(errs, closer.Close())
		}
		t.closeErr = errors.Join(errs...)
	})
	return t.closeErr
}

func (t *LineTransport) operationReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closed:
		return ErrTransportDone
	default:
		return nil
	}
}

func (t *LineTransport) drainFrame() error {
	for {
		_, err := t.reader.ReadSlice('\n')
		switch {
		case err == nil, errors.Is(err, io.EOF):
			return nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return err
		}
	}
}

func trimLineEnding(line []byte) []byte {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
	}
	return line
}
