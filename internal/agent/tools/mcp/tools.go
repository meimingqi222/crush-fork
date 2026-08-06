package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Tool = mcp.Tool

// ToolMedia represents a single media payload returned by an MCP tool.
type ToolMedia struct {
	Type      string
	Data      []byte
	MediaType string
}

// ToolResult represents the result of running an MCP tool.
type ToolResult struct {
	Type            string
	Content         string
	Data            []byte
	MediaType       string
	AdditionalMedia []ToolMedia
}

var (
	allTools          = csync.NewMap[string, []*Tool]()
	callToolOnSession = func(ctx context.Context, session *ClientSession, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return session.CallTool(ctx, params)
	}
	reconnectClient = Reconnect
	refreshToken    = RefreshToken
	// reconnectRetryDelay is the backoff applied before a synchronous
	// reconnect triggered by a failed tool call. It only gates the
	// network-error retry path; the OAuth refresh path retries immediately.
	// Tests may shorten it to run quickly.
	reconnectRetryDelay = 100 * time.Millisecond
)

// Tools returns all available MCP tools.
func Tools() iter.Seq2[string, []*Tool] {
	return allTools.Seq2()
}

// RunTool runs an MCP tool with the given input parameters.
func RunTool(ctx context.Context, cfg *config.ConfigStore, name, toolName string, input string) (ToolResult, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return ToolResult{}, fmt.Errorf("error parsing parameters: %s", err)
	}

	// If the server is not currently connected (e.g. its tools were loaded
	// from cache after a connection failure, or no session exists yet), try
	// to establish a live connection before invoking the tool. This avoids a
	// hard failure when the LLM picks a cached tool whose server is still
	// coming up.
	if !isSessionReady(name) {
		if isReconnecting(name) {
			// A background reconnect loop is already running. Wait for it
			// to re-establish the session rather than triggering a
			// redundant synchronous reconnect that would race with the
			// loop and risk a reconnect storm when multiple tools are
			// called concurrently against the same server.
			if !waitForSessionReady(ctx, name, 10*time.Second) {
				return ToolResult{}, errors.New("server still connecting, please retry")
			}
		} else {
			waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := reconnectClient(waitCtx, cfg, name); err != nil {
				slog.Warn("RunTool initial reconnect failed", "name", name, "error", err)
			}
			cancel()
			if !isSessionReady(name) {
				return ToolResult{}, errors.New("server still connecting, please retry")
			}
		}
	}

	c, err := getOrRenewClient(ctx, cfg, name)
	if err != nil {
		return ToolResult{}, err
	}
	result, err := callToolWithRetry(ctx, cfg, name, c, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return ToolResult{}, err
	}

	if len(result.Content) == 0 {
		return ToolResult{Type: "text", Content: ""}, nil
	}

	return resultFromMCPContent(result.Content), nil
}

// isSessionReady reports whether the named MCP server has a live, connected
// session ready to accept tool calls.
func isSessionReady(name string) bool {
	info, ok := states.Get(name)
	if !ok || info.State != StateConnected {
		return false
	}
	_, hasSession := sessions.Get(name)
	return hasSession
}

// waitForSessionReady polls isSessionReady until it returns true, the
// context is cancelled, or the timeout elapses. It is used by RunTool to
// wait for a background reconnect loop to re-establish a session without
// triggering a redundant synchronous reconnect that would race with the
// loop.
func waitForSessionReady(ctx context.Context, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if isSessionReady(name) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func callToolWithRetry(ctx context.Context, cfg *config.ConfigStore, name string, session *ClientSession, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	result, err := callToolOnSession(ctx, session, params)
	if err == nil {
		return result, nil
	}

	// On 401/403 auth errors, try refreshing the OAuth token before giving
	// up. If refresh succeeds, retry the tool call transparently so the LLM
	// never sees the auth failure. If refresh fails, mark the server as
	// needing reauthentication and return a friendly error.
	if ctx.Err() == nil && stateForError(err) == StateNeedsAuth {
		if refreshErr := refreshToken(ctx, name); refreshErr == nil {
			result, err = callToolOnSession(ctx, session, params)
			if err != nil {
				updateStateForToolCallError(name, err)
			}
			return result, err
		}
		// Refresh failed — surface as an auth error requiring user action.
		updateStateForToolCallError(name, err)
		return nil, fmt.Errorf("mcp %s requires reauthentication: %w", name, err)
	}

	if !shouldRetryToolCall(ctx, err) {
		updateStateForToolCallError(name, err)
		return result, err
	}

	firstErr := err
	updateStateForToolCallError(name, firstErr)

	// Single reconnect limit: if a background reconnectLoop is already
	// running for this server, do not trigger a redundant synchronous
	// reconnect that would race with it. Surface the original failure and
	// let the background loop re-establish the connection.
	if isReconnecting(name) {
		return nil, firstErr
	}

	// Back off briefly before reconnecting so the transport has a chance to
	// settle and concurrent callers do not hammer a flapping server. This
	// delay applies only to the network-error retry path; the OAuth refresh
	// path above retries without waiting.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(reconnectRetryDelay):
	}

	if err := reconnectClient(ctx, cfg, name); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.Join(firstErr, err)
	}

	session, ok := sessions.Get(name)
	if !ok {
		return nil, firstErr
	}

	result, err = callToolOnSession(ctx, session, params)
	if err != nil {
		updateStateForToolCallError(name, err)
	}
	return result, err
}

func updateStateForToolCallError(name string, err error) {
	if prev, ok := states.Get(name); ok {
		updateState(name, stateForError(err), err, nil, prev.Counts)
	}
}

func shouldRetryToolCall(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, mcp.ErrSessionMissing) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// Recognize low-level connection failures that surface as syscall errors
	// on both Unix and Windows. These are transient and worth a reconnect.
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func resultFromMCPContent(content []mcp.Content) ToolResult {
	var textParts []string
	mediaParts := make([]ToolMedia, 0, len(content))

	for _, v := range content {
		switch item := v.(type) {
		case *mcp.TextContent:
			textParts = append(textParts, item.Text)
		case *mcp.ImageContent:
			mediaParts = append(mediaParts, ToolMedia{
				Type:      "image",
				Data:      ensureBase64(item.Data),
				MediaType: item.MIMEType,
			})
		case *mcp.AudioContent:
			mediaParts = append(mediaParts, ToolMedia{
				Type:      "media",
				Data:      ensureBase64(item.Data),
				MediaType: item.MIMEType,
			})
		default:
			textParts = append(textParts, fmt.Sprintf("%v", v))
		}
	}

	textContent := strings.Join(textParts, "\n")
	if len(mediaParts) == 0 {
		return ToolResult{Type: "text", Content: textContent}
	}

	result := ToolResult{
		Type:      mediaParts[0].Type,
		Content:   textContent,
		Data:      mediaParts[0].Data,
		MediaType: mediaParts[0].MediaType,
	}
	if len(mediaParts) > 1 {
		result.AdditionalMedia = append(result.AdditionalMedia, mediaParts[1:]...)
	}
	return result
}

// RefreshTools gets the updated list of tools from the MCP and updates the
// global state.
func RefreshTools(ctx context.Context, cfg *config.ConfigStore, name string) {
	session, ok := sessions.Get(name)
	if !ok {
		slog.Warn("Refresh tools: no session", "name", name)
		return
	}

	tools, err := getTools(ctx, session)
	if err != nil {
		updateState(name, stateForError(err), err, nil, Counts{})
		return
	}

	toolCount := updateTools(cfg, name, tools)

	prev, _ := states.Get(name)
	prev.Counts.Tools = toolCount
	updateState(name, StateConnected, nil, session, prev.Counts)
}

func getTools(ctx context.Context, session *ClientSession) ([]*Tool, error) {
	// Always call ListTools to get the actual available tools.
	// The InitializeResult Capabilities.Tools field may be an empty object {},
	// which is valid per MCP spec, but we still need to call ListTools to discover tools.
	result, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	return result.Tools, nil
}

func updateTools(cfg *config.ConfigStore, name string, tools []*Tool) int {
	tools = filterTools(cfg, name, tools)
	if len(tools) == 0 {
		allTools.Del(name)
		return 0
	}
	allTools.Set(name, tools)
	return len(tools)
}

// filterTools removes tools that are disabled or not enabled (whitelisted) via config.
func filterTools(cfg *config.ConfigStore, mcpName string, tools []*Tool) []*Tool {
	mcpCfg, ok := cfg.GetMCP(mcpName)
	if !ok {
		return tools
	}

	hasEnabled := len(mcpCfg.EnabledTools) > 0
	hasDisabled := len(mcpCfg.DisabledTools) > 0
	if !hasEnabled && !hasDisabled {
		return tools
	}

	filtered := make([]*Tool, 0, len(tools))
	for _, tool := range tools {
		if hasEnabled && !slices.Contains(mcpCfg.EnabledTools, tool.Name) {
			continue
		}
		if hasDisabled && slices.Contains(mcpCfg.DisabledTools, tool.Name) {
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

// ensureBase64 normalizes valid base64 input and guarantees padded
// base64.StdEncoding output; otherwise it encodes raw binary data.
func ensureBase64(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	normalized := normalizeBase64Input(data)
	if decoded, ok := decodeBase64(normalized); ok {
		encoded := make([]byte, base64.StdEncoding.EncodedLen(len(decoded)))
		base64.StdEncoding.Encode(encoded, decoded)
		return encoded
	}

	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(data)))
	base64.StdEncoding.Encode(encoded, data)
	return encoded
}

func normalizeBase64Input(data []byte) []byte {
	normalized := strings.Join(strings.Fields(string(data)), "")
	return []byte(normalized)
}

func decodeBase64(data []byte) ([]byte, bool) {
	if len(data) == 0 {
		return data, true
	}

	for _, b := range data {
		if b > 127 {
			return nil, false
		}
	}

	s := string(data)
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err == nil {
		return decoded, true
	}

	// For RawStdEncoding (no padding), apply stricter heuristics to reduce
	// false positives. We require:
	// 1. Minimum length of 8 characters (more likely to be real base64)
	// 2. Length must be a multiple of 4 when padding is added (base64 alignment)
	// This reduces the chance of misinterpreting random ASCII text as base64.
	if len(s) >= 8 && len(s)%4 == 0 {
		decoded, err = base64.RawStdEncoding.DecodeString(s)
		if err == nil {
			return decoded, true
		}
	}
	return nil, false
}

// isValidBase64 checks if the data appears to be valid base64-encoded content.
func isValidBase64(data []byte) bool {
	if len(data) == 0 {
		return true
	}

	for _, b := range data {
		if b > 127 {
			return false
		}
	}

	s := string(data)
	if _, err := base64.StdEncoding.DecodeString(s); err == nil {
		return true
	}
	_, err := base64.RawStdEncoding.DecodeString(s)
	return err == nil
}
