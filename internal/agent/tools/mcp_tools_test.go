package tools

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeMCPMediaPayloadDecodesMediaType(t *testing.T) {
	t.Parallel()

	raw := []byte("image-bytes")
	encoded := []byte(base64.StdEncoding.EncodeToString(raw))

	got, mime := normalizeMCPMediaPayload("media", encoded, "image/png", "test-tool")
	require.Equal(t, raw, got)
	require.Equal(t, "image/png", mime)
}
