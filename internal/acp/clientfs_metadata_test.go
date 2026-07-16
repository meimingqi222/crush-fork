package acp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClientFSClientMetadataIsBoundedAndRedacted(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(map[string]any{
		"file_path": "main.go", "source_uri": "file:///main.go", "revision": "r1",
		"old_content": "do not expose", "new_content": "also secret",
	})
	require.NoError(t, err)
	metadata := clientFSClientMetadata(string(raw))
	projected, err := json.Marshal(metadata)
	require.NoError(t, err)
	require.Contains(t, string(projected), "file:///main.go")
	require.Contains(t, string(projected), `"revision":"r1"`)
	require.NotContains(t, string(projected), "secret")
	require.NotContains(t, string(projected), "old_content")
}

func TestRPCErrorPreservesStructuredClientFSCode(t *testing.T) {
	t.Parallel()
	err := &RPCError{
		Code: -32034, Message: "stale write",
		Data: map[string]any{"code": "CRUSH_REVISION_CONFLICT", "retryable": false},
	}
	require.Equal(t, "CRUSH_REVISION_CONFLICT", err.ClientFSCode())
	require.Contains(t, err.Error(), "stale write")
}
