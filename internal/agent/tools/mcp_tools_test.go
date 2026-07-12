package tools

import (
	"context"
	"encoding/base64"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

type testMCPServerAccess struct {
	allowed  bool
	scope    string
	revision uint64
}

func (a *testMCPServerAccess) AllowsMCPServer(string) bool { return a.allowed }
func (a *testMCPServerAccess) MCPServerScope() string      { return a.scope }
func (a *testMCPServerAccess) MCPServerRevision() uint64   { return a.revision }

func TestNormalizeMCPMediaPayloadDecodesMediaType(t *testing.T) {
	t.Parallel()

	raw := []byte("image-bytes")
	encoded := []byte(base64.StdEncoding.EncodeToString(raw))

	got, mime := normalizeMCPMediaPayload("media", encoded, "image/png", "test-tool")
	require.Equal(t, raw, got)
	require.Equal(t, "image/png", mime)
}

func TestMCPToolRunReauthorizesAgainstLiveAccess(t *testing.T) {
	t.Parallel()

	access := &testMCPServerAccess{allowed: true, scope: "session-a", revision: 1}
	ctx := WithMCPServerAccess(context.Background(), access)
	require.True(t, MCPServerAllowed(ctx, "dynamic-server"))
	require.Equal(t, "session-a", MCPServerAccessScope(ctx))
	require.Equal(t, uint64(1), MCPServerAccessRevision(ctx))

	// Simulate replacement or shutdown after a provider step retained an old
	// tool object. Invocation must consult the live capability again.
	access.allowed = false
	access.revision++
	response, err := (&Tool{mcpName: "dynamic-server"}).Run(ctx, fantasy.ToolCall{})

	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "not available in this session")
	require.Equal(t, uint64(2), MCPServerAccessRevision(ctx))
}

func TestMCPServerAllowedWithoutScopeRejectsScopedServers(t *testing.T) {
	t.Parallel()

	name := "dynamic-server-" + t.Name()
	MarkMCPServerScoped(name)

	require.False(t, MCPServerAllowed(context.Background(), name))
	require.True(t, MCPServerAllowed(context.Background(), "static-server-"+t.Name()))
}
