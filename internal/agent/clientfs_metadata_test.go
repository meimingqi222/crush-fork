package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClientFSFilesFromMetadataProjectsOnlySourceIdentity(t *testing.T) {
	t.Parallel()
	metadata, err := json.Marshal(map[string]any{
		"file_path": "main.go", "source_uri": "untitled:///main.go", "revision": "buffer:9",
		"old_content": "secret old", "new_content": "secret new",
	})
	require.NoError(t, err)
	files := clientFSFilesFromMetadata(string(metadata))
	require.Len(t, files, 1)
	require.Equal(t, "main.go", files[0].Path)
	require.Equal(t, "untitled:///main.go", files[0].SourceURI)
	require.Equal(t, "buffer:9", files[0].Revision)
	projected, err := json.Marshal(files)
	require.NoError(t, err)
	require.NotContains(t, string(projected), "secret")
}
