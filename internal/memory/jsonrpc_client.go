package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// JSONRPCClient is a stdio JSON-RPC 2.0 client for communicating with the universal-memory runtime.
type JSONRPCClient struct {
	stdin  io.Writer
	stdout io.Reader
	stderr io.Writer

	nextID  int
	pending map[int]chan *JSONRPCResponse
	mu      sync.Mutex
}

// JSONRPCRequest represents a JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// JSONRPCResponse represents a JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

// JSONRPCError represents a JSON-RPC 2.0 error.
type JSONRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *JSONRPCError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// NewJSONRPCClient creates a new JSON-RPC client.
func NewJSONRPCClient(stdin io.Writer, stdout, stderr io.Reader) *JSONRPCClient {
	return &JSONRPCClient{
		stdin:   stdin,
		stdout:  stdout,
		stderr:  io.Discard, // stderr is read-only, use io.Discard for logging
		nextID:  1,
		pending: make(map[int]chan *JSONRPCResponse),
	}
}

// Start begins reading responses from stdout.
func (c *JSONRPCClient) Start(ctx context.Context) {
	go c.readResponses(ctx)
}

// readResponses continuously reads responses from stdout.
func (c *JSONRPCClient) readResponses(ctx context.Context) {
	scanner := bufio.NewScanner(c.stdout)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		var resp JSONRPCResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			// Log parse errors to stderr via runtime process
			continue
		}

		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()

		if ok {
			ch <- &resp
		}
	}

	// Scanner error is ignored - connection closed
}

// Call sends a JSON-RPC request and waits for the response.
func (c *JSONRPCClient) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	ch := make(chan *JSONRPCResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	if _, err := fmt.Fprintln(c.stdin, string(reqBytes)); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, &JSONRPCError{
				Code:    resp.Error.Code,
				Message: resp.Error.Message,
				Data:    resp.Error.Data,
			}
		}
		return resp.Result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}
